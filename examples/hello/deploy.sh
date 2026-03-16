#!/usr/bin/env bash
#
# Deploy the hello app to blockyard.
#
# Automates the full flow:
#   1. OIDC login via Dex (using static demo credentials)
#   2. PAT creation via the blockyard API
#   3. App creation, bundle upload, and start
#
# Prerequisites:
#   - docker compose up -d
#
set -euo pipefail

BASE_URL="${BLOCKYARD_URL:-http://localhost:8080}"
DEX_URL="${DEX_URL:-http://localhost:5556}"
DEX_EMAIL="demo@example.com"
DEX_PASSWORD="password"
APP_NAME="hello"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="${SCRIPT_DIR}/app"
COOKIE_JAR="$(mktemp /tmp/blockyard-cookies-XXXXXX)"
trap 'rm -f "${COOKIE_JAR}"' EXIT

# ── Wait for services ────────────────────────────────────────────────

echo "==> Waiting for blockyard to be ready..."
for i in $(seq 1 30); do
  if curl -sf "${BASE_URL}/healthz" > /dev/null 2>&1; then break; fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: blockyard did not become healthy" >&2; exit 1
  fi
  sleep 1
done
echo "    OK"

# ── Automated OIDC login ────────────────────────────────────────────
#
# Replicate what a browser does:
#   GET /login → 302 to Dex auth → Dex login form → POST credentials
#   → 302 back to /callback → session cookie set
#

echo "==> Logging in via Dex..."

# Step 1: Hit /login, follow all redirects to reach the Dex login form.
# This sets the blockyard_oidc_state cookie and ends on Dex's login page.
LOGIN_PAGE=$(curl -sS -L -c "${COOKIE_JAR}" -b "${COOKIE_JAR}" \
  -w '\n%{url_effective}' \
  "${BASE_URL}/login")

EFFECTIVE_URL=$(echo "$LOGIN_PAGE" | tail -1)
LOGIN_HTML=$(echo "$LOGIN_PAGE" | sed '$d')

# Step 2: Extract the form action URL from the Dex login page.
# Dex renders: <form method="post" action="/auth/local/login?back=...&amp;req=...">
FORM_ACTION=$(echo "$LOGIN_HTML" | grep -o 'action="[^"]*"' | head -1 | cut -d'"' -f2 | sed 's/&amp;/\&/g')

if [ -z "$FORM_ACTION" ]; then
  echo "ERROR: Could not find login form on Dex page" >&2
  echo "    Effective URL: ${EFFECTIVE_URL}" >&2
  exit 1
fi

# If the action is a relative path, prepend the Dex origin.
if [[ "$FORM_ACTION" == /* ]]; then
  FORM_ACTION="${DEX_URL}${FORM_ACTION}"
fi

# Step 3: POST credentials to Dex, follow redirects back to blockyard's
# /callback which sets the blockyard_session cookie.
CALLBACK_RESP=$(curl -sS -L -c "${COOKIE_JAR}" -b "${COOKIE_JAR}" \
  -w '\n%{http_code}' \
  -d "login=${DEX_EMAIL}&password=${DEX_PASSWORD}" \
  "${FORM_ACTION}")

CALLBACK_CODE=$(echo "$CALLBACK_RESP" | tail -1)
if ! grep -q 'blockyard_session' "${COOKIE_JAR}" 2>/dev/null; then
  echo "ERROR: Login failed — no session cookie received (HTTP ${CALLBACK_CODE})" >&2
  exit 1
fi

echo "    OK"

# ── Create a Personal Access Token ──────────────────────────────────

echo "==> Creating Personal Access Token..."
TOKEN_RESP=$(curl -sS -b "${COOKIE_JAR}" \
  -X POST -H "Content-Type: application/json" \
  -d '{"name":"deploy-script","expires_in":"1d"}' \
  "${BASE_URL}/api/v1/users/me/tokens")

TOKEN=$(echo "$TOKEN_RESP" | grep -o '"token":"[^"]*' | cut -d'"' -f4)
if [ -z "$TOKEN" ]; then
  echo "ERROR: Failed to create PAT" >&2
  echo "    Response: ${TOKEN_RESP}" >&2
  exit 1
fi

echo "    OK (expires in 24h)"

# ── Helper: authenticated curl ──────────────────────────────────────

auth() {
  curl -sf -H "Authorization: Bearer ${TOKEN}" "$@"
}

# ── Create the app (ignore 409 if it already exists) ────────────────

echo "==> Creating app '${APP_NAME}'..."
create_resp=$(auth -w "\n%{http_code}" -X POST "${BASE_URL}/api/v1/apps" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"${APP_NAME}\"}" 2>/dev/null || true)
http_code=$(echo "$create_resp" | tail -1)
body=$(echo "$create_resp" | sed '$d')

if [ "$http_code" = "201" ]; then
  APP_ID=$(echo "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "    Created (id=${APP_ID})"
elif [ "$http_code" = "409" ]; then
  echo "    Already exists, fetching..."
  list_resp=$(auth "${BASE_URL}/api/v1/apps")
  APP_ID=$(echo "$list_resp" | grep -o "\"id\":\"[^\"]*\",\"name\":\"${APP_NAME}\"" | head -1 | cut -d'"' -f4)
  echo "    Found (id=${APP_ID})"
else
  echo "ERROR: Failed to create app (HTTP ${http_code})" >&2
  echo "$body" >&2
  exit 1
fi

# ── Bundle the app as tar.gz ────────────────────────────────────────

echo "==> Bundling app..."
BUNDLE_FILE="$(mktemp /tmp/hello-XXXXXX).tar.gz"
tar -czf "${BUNDLE_FILE}" -C "${APP_DIR}" .
echo "    ${BUNDLE_FILE} ($(wc -c < "${BUNDLE_FILE}" | tr -d ' ') bytes)"

# ── Upload the bundle ───────────────────────────────────────────────

echo "==> Uploading bundle..."
upload_resp=$(auth -X POST "${BASE_URL}/api/v1/apps/${APP_ID}/bundles" \
  -H "Content-Type: application/octet-stream" \
  --data-binary "@${BUNDLE_FILE}")
rm -f "${BUNDLE_FILE}"

TASK_ID=$(echo "$upload_resp" | grep -o '"task_id":"[^"]*"' | head -1 | cut -d'"' -f4)
BUNDLE_ID=$(echo "$upload_resp" | grep -o '"bundle_id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    bundle=${BUNDLE_ID}"
echo "    task=${TASK_ID}"

# ── Stream build logs until the task finishes ───────────────────────

echo "==> Restoring dependencies (this may take a while on first run)..."
auth "${BASE_URL}/api/v1/tasks/${TASK_ID}/logs"

task_resp=$(auth "${BASE_URL}/api/v1/tasks/${TASK_ID}" 2>/dev/null || true)
status=$(echo "$task_resp" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ "$status" != "completed" ]; then
  echo "ERROR: Restore failed (status=${status})" >&2
  exit 1
fi
echo "    Restore complete!"

# ── Start the app ───────────────────────────────────────────────────

echo "==> Starting app..."
auth -X POST "${BASE_URL}/api/v1/apps/${APP_ID}/start" > /dev/null

echo ""
echo "Done! Open ${BASE_URL}/app/${APP_NAME}/ in your browser."
echo "You will be redirected to Dex to log in."
echo ""
echo "  Email:    ${DEX_EMAIL}"
echo "  Password: ${DEX_PASSWORD}"
echo ""
