#!/usr/bin/env bash
#
# Deploy the hello-shiny app to a local blockyard instance.
#
# Usage:
#   # 1. Start blockyard
#   docker compose up -d
#
#   # 2. Wait for it to be healthy, then deploy
#   ./deploy.sh
#
set -euo pipefail

BASE_URL="${BLOCKYARD_URL:-http://localhost:8080}"
TOKEN="${BLOCKYARD_TOKEN:-my-secret-token}"
APP_NAME="hello-shiny"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

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
BUNDLE_FILE=$(mktemp /tmp/hello-shiny-XXXXXX.tar.gz)
tar -czf "${BUNDLE_FILE}" -C "${SCRIPT_DIR}/app" .
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

# --- Poll the task until done ---
echo "==> Restoring dependencies (this may take a while on first run)..."
while true; do
  task_resp=$(auth "${BASE_URL}/api/v1/tasks/${TASK_ID}/logs" 2>/dev/null || true)
  if echo "$task_resp" | grep -q '"done":true'; then
    if echo "$task_resp" | grep -q '"success":true'; then
      echo "    Restore complete!"
    else
      echo "ERROR: Restore failed. Check task logs:" >&2
      echo "$task_resp" >&2
      exit 1
    fi
    break
  fi
  sleep 2
done

# --- Start the app ---
echo "==> Starting app..."
auth -X POST "${BASE_URL}/api/v1/apps/${APP_ID}/start" > /dev/null

echo ""
echo "Done! App available at: ${BASE_URL}/app/${APP_NAME}/"
echo ""
