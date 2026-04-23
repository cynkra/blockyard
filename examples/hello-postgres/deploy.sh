#!/usr/bin/env bash
#
# Deploy the hello-postgres app to blockyard (direct PostgreSQL board
# storage with per-user vault-managed credentials).
#
# Prerequisites:
#   - docker compose up -d --wait
#   - by CLI installed
#
set -euo pipefail

export BLOCKYARD_URL="${BLOCKYARD_URL:-http://localhost:8080}"
BOOTSTRAP_TOKEN="${BLOCKYARD_BOOTSTRAP_TOKEN:-by_bootstrap_for_examples}"
APP_NAME="hello-postgres"
DEX_EMAIL="demo@example.com"
DEX_PASSWORD="password"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APP_DIR="${SCRIPT_DIR}/app"

# Exchange bootstrap token for a real PAT.
export BLOCKYARD_TOKEN=$(curl -sS -X POST \
  -H "Authorization: Bearer ${BOOTSTRAP_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"name":"deploy-script","expires_in":"1h"}' \
  "${BLOCKYARD_URL}/api/v1/bootstrap" | grep -o '"token":"[^"]*' | cut -d'"' -f4)

if [ -z "${BLOCKYARD_TOKEN}" ]; then
  echo "ERROR: failed to exchange bootstrap token" >&2
  exit 1
fi

by deploy "${APP_DIR}" --yes --wait --name "${APP_NAME}"
by access set-type "${APP_NAME}" logged_in
by enable "${APP_NAME}"

echo ""
echo "Done! Open ${BLOCKYARD_URL}/app/${APP_NAME}/ in your browser."
echo "You will be redirected to Dex to log in."
echo ""
echo "  User 1:  ${DEX_EMAIL} / ${DEX_PASSWORD}"
echo "  User 2:  demo2@example.com / ${DEX_PASSWORD}"
echo ""
echo "Board storage talks directly to PostgreSQL with RLS; per-user"
echo "DB credentials are minted and rotated by vault. Blockyard is not"
echo "in the runtime auth or data path."
echo ""
echo "  PostgreSQL: postgres://<user_entity-id>:<rotated>@localhost:5432/blockyard"
echo "  OpenBao:    http://localhost:8200"
echo ""
