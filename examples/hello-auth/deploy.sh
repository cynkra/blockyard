#!/usr/bin/env bash
#
# Deploy the hello-shiny app to a blockyard instance with OIDC + OpenBao.
#
# Usage:
#   # 1. Start the stack
#   docker compose up -d
#
#   # 2. Log in via browser, create a PAT at /api/v1/users/me/tokens
#   export BLOCKYARD_TOKEN=by_...
#
#   # 3. Deploy the app
#   ./deploy.sh
#
#   # 4. Open in browser (requires "dex" in /etc/hosts)
#   open http://localhost:8080/app/hello-shiny/
#
set -euo pipefail

BASE_URL="${BLOCKYARD_URL:-http://localhost:8080}"
TOKEN="${BLOCKYARD_TOKEN:?Set BLOCKYARD_TOKEN to a Personal Access Token (by_...)}"
APP_NAME="hello-shiny"

# Resolve the app directory (reuse hello-shiny example app).
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="${SCRIPT_DIR}/../hello-shiny/app"
if [ ! -d "$APP_DIR" ]; then
  echo "ERROR: app directory not found at ${APP_DIR}" >&2
  echo "Make sure the hello-shiny example is present." >&2
  exit 1
fi

auth() {
  curl -sf -H "Authorization: Bearer ${TOKEN}" "$@"
}

echo "==> Waiting for blockyard to be ready..."
for i in $(seq 1 30); do
  if curl -sf "${BASE_URL}/healthz" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: blockyard did not become healthy" >&2
    exit 1
  fi
  sleep 1
done
echo "    OK"

# --- Create the app (ignore 409 if it already exists) ---
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

# --- Bundle the app as tar.gz ---
echo "==> Bundling app..."
BUNDLE_FILE="$(mktemp /tmp/hello-shiny-XXXXXX).tar.gz"
tar -czf "${BUNDLE_FILE}" -C "${APP_DIR}" .
echo "    ${BUNDLE_FILE} ($(wc -c < "${BUNDLE_FILE}" | tr -d ' ') bytes)"

# --- Upload the bundle ---
echo "==> Uploading bundle..."
upload_resp=$(auth -X POST "${BASE_URL}/api/v1/apps/${APP_ID}/bundles" \
  -H "Content-Type: application/octet-stream" \
  --data-binary "@${BUNDLE_FILE}")
rm -f "${BUNDLE_FILE}"

TASK_ID=$(echo "$upload_resp" | grep -o '"task_id":"[^"]*"' | head -1 | cut -d'"' -f4)
BUNDLE_ID=$(echo "$upload_resp" | grep -o '"bundle_id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    bundle=${BUNDLE_ID}"
echo "    task=${TASK_ID}"

# --- Stream build logs until the task finishes ---
echo "==> Restoring dependencies (this may take a while on first run)..."
auth "${BASE_URL}/api/v1/tasks/${TASK_ID}/logs"

task_resp=$(auth "${BASE_URL}/api/v1/tasks/${TASK_ID}" 2>/dev/null || true)
status=$(echo "$task_resp" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ "$status" != "completed" ]; then
  echo "ERROR: Restore failed (status=${status})" >&2
  exit 1
fi
echo "    Restore complete!"

# --- Start the app ---
echo "==> Starting app..."
auth -X POST "${BASE_URL}/api/v1/apps/${APP_ID}/start" > /dev/null

echo ""
echo "Done! Open http://localhost:8080/app/${APP_NAME}/ in your browser."
echo "You will be redirected to Dex to log in."
echo ""
echo "  Email:    demo@example.com"
echo "  Password: password"
echo ""
