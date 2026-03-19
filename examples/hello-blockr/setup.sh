#!/usr/bin/env sh
#
# Combined init: configure OpenBao and PocketBase, and pre-enroll
# PocketBase credentials for both demo users.
#
# Runs as a one-shot init container after OpenBao, Dex, and PocketBase
# are started.
#
set -eu

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
BAO_TOKEN="${BAO_TOKEN:-root-dev-token}"
DEX_ISSUER="${DEX_ISSUER:-http://localhost:5556}"
DEX_INTERNAL="${DEX_INTERNAL:-${DEX_ISSUER}}"
APPROLE_SECRET_ID="${APPROLE_SECRET_ID:-dev-secret-id-for-local-use-only}"
PB_URL="${PB_URL:-http://pocketbase:8090}"
PB_ADMIN_EMAIL="${PB_ADMIN_EMAIL:-admin@pocketbase.local}"
PB_ADMIN_PASSWORD="${PB_ADMIN_PASSWORD:-pb-admin-dev-password}"
PB_USER_PASSWORD="${PB_USER_PASSWORD:-demo-password}"
DEMO_SUB="${DEMO_SUB:-Cg1kZW1vLXVzZXItMDAxEgVsb2NhbA}"
DEMO2_SUB="${DEMO2_SUB:-Cg1kZW1vLXVzZXItMDAyEgVsb2NhbA}"

bao_header="-H X-Vault-Token:${BAO_TOKEN} -H Content-Type:application/json"

bao_post() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -sf $bao_header -X POST "${BAO_ADDR}${path}" "$@"
}

# ══════════════════════════════════════════════════════════════════════
# OpenBao setup (JWT auth + AppRole)
# ══════════════════════════════════════════════════════════════════════

echo "==> Waiting for Dex JWKS..."
for i in $(seq 1 60); do
  if curl -sf "${DEX_INTERNAL}/keys" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "ERROR: Dex did not become ready" >&2
    exit 1
  fi
  sleep 1
done
echo "    OK"

echo "==> Enabling JWT auth method..."
bao_post /v1/sys/auth/jwt -d '{"type":"jwt"}' || true
echo "    OK"

echo "==> Configuring JWT auth with Dex..."
bao_post /v1/auth/jwt/config -d "{
  \"jwks_url\":      \"${DEX_INTERNAL}/keys\",
  \"bound_issuer\":  \"${DEX_ISSUER}\",
  \"default_role\":  \"blockyard-user\"
}"
echo "    OK"

echo "==> Creating blockyard-user policy..."
bao_post /v1/sys/policy/blockyard-user -d '{
  "policy": "path \"secret/data/users/{{identity.entity.aliases.auth_jwt_*.name}}/*\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/lookup-self\" {\n  capabilities = [\"read\"]\n}"
}'
echo "    OK"

echo "==> Creating blockyard-user role..."
bao_post /v1/auth/jwt/role/blockyard-user -d '{
  "role_type":       "jwt",
  "bound_audiences": ["blockyard"],
  "user_claim":      "sub",
  "claim_mappings":  {"sub": "sub"},
  "token_policies":  ["blockyard-user"],
  "token_ttl":       "1h"
}'
echo "    OK"

echo "==> Enabling AppRole auth method..."
bao_post /v1/sys/auth/approle -d '{"type":"approle"}' || true
echo "    OK"

echo "==> Creating blockyard-server policy..."
bao_post /v1/sys/policy/blockyard-server -d '{
  "policy": "path \"secret/data/blockyard/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\"]\n}\npath \"secret/metadata/blockyard/*\" {\n  capabilities = [\"read\", \"list\"]\n}\npath \"sys/auth\" {\n  capabilities = [\"read\"]\n}\npath \"sys/mounts\" {\n  capabilities = [\"read\"]\n}\npath \"sys/policies/acl/*\" {\n  capabilities = [\"read\"]\n}\npath \"auth/jwt/role/blockyard-user\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/renew-self\" {\n  capabilities = [\"update\"]\n}\npath \"secret/data/users/*\" {\n  capabilities = [\"create\", \"update\"]\n}\npath \"secret/metadata/users/*\" {\n  capabilities = [\"read\"]\n}"
}'
echo "    OK"

echo "==> Creating blockyard-server AppRole role..."
bao_post /v1/auth/approle/role/blockyard-server -d '{
  "token_policies":  ["blockyard-server"],
  "token_ttl":       "1h",
  "token_max_ttl":   "0"
}'
echo "    OK"

echo "==> Setting custom role_id for dev..."
bao_post /v1/auth/approle/role/blockyard-server/role-id -d '{"role_id": "blockyard-server"}' || true
echo "    OK"

echo "==> Setting custom secret_id for dev..."
bao_post /v1/auth/approle/role/blockyard-server/custom-secret-id -d "{
  \"secret_id\": \"${APPROLE_SECRET_ID}\"
}" || true
echo "    OK"

echo "==> OpenBao configured."

# ══════════════════════════════════════════════════════════════════════
# PocketBase setup (superuser, users, boards collection)
# ══════════════════════════════════════════════════════════════════════

echo "==> Waiting for PocketBase..."
for i in $(seq 1 60); do
  if curl -sf "${PB_URL}/api/health" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "ERROR: PocketBase did not become ready" >&2
    exit 1
  fi
  sleep 1
done
echo "    OK"

echo "==> Creating PocketBase superuser..."
curl -sf -X POST "${PB_URL}/api/collections/_superusers/records" \
  -H "Content-Type: application/json" \
  -d "{
    \"email\":           \"${PB_ADMIN_EMAIL}\",
    \"password\":        \"${PB_ADMIN_PASSWORD}\",
    \"passwordConfirm\": \"${PB_ADMIN_PASSWORD}\"
  }" > /dev/null || true
echo "    OK"

echo "==> Authenticating as PocketBase superuser..."
PB_AUTH=$(curl -sf -X POST "${PB_URL}/api/collections/_superusers/auth-with-password" \
  -H "Content-Type: application/json" \
  -d "{\"identity\":\"${PB_ADMIN_EMAIL}\",\"password\":\"${PB_ADMIN_PASSWORD}\"}")
PB_TOKEN=$(echo "$PB_AUTH" | grep -o '"token":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -z "$PB_TOKEN" ]; then
  echo "ERROR: Failed to authenticate as PocketBase superuser" >&2
  exit 1
fi
echo "    OK"

pb_header="-H Authorization:${PB_TOKEN} -H Content-Type:application/json"

pb_req() {
  method="$1"; path="$2"; shift 2
  # shellcheck disable=SC2086
  curl -sf $pb_header -X "$method" "${PB_URL}${path}" "$@"
}

echo "==> Creating demo users in PocketBase..."
pb_req POST /api/collections/users/records \
  -d "{
    \"email\":           \"demo@example.com\",
    \"password\":        \"${PB_USER_PASSWORD}\",
    \"passwordConfirm\": \"${PB_USER_PASSWORD}\",
    \"name\":            \"Demo User\",
    \"emailVisibility\":  true
  }" > /dev/null || true
pb_req POST /api/collections/users/records \
  -d "{
    \"email\":           \"demo2@example.com\",
    \"password\":        \"${PB_USER_PASSWORD}\",
    \"passwordConfirm\": \"${PB_USER_PASSWORD}\",
    \"name\":            \"Demo User 2\",
    \"emailVisibility\":  true
  }" > /dev/null || true
echo "    OK"

echo "==> Updating users collection rules for user discovery..."
pb_req PATCH /api/collections/users \
  -d '{"listRule":"@request.auth.id != \"\"","viewRule":"@request.auth.id != \"\""}' \
  > /dev/null
echo "    OK"

echo "==> Getting users collection ID..."
USERS_COL=$(pb_req GET /api/collections/users)
USERS_ID=$(echo "$USERS_COL" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    id=${USERS_ID}"

echo "==> Creating boards collection..."
BOARDS_PAYLOAD=$(cat <<PAYLOAD
{
  "name": "boards",
  "type": "base",
  "fields": [
    {"name": "name", "type": "text", "required": true},
    {"name": "owner", "type": "relation", "required": true, "collectionId": "${USERS_ID}", "maxSelect": 1},
    {"name": "shared_with", "type": "relation", "collectionId": "${USERS_ID}"},
    {"name": "acl_type", "type": "select", "required": true, "options": {"values": ["private", "restricted", "public"]}},
    {"name": "tags", "type": "json"}
  ],
  "listRule": "owner = @request.auth.id || (acl_type = 'restricted' && shared_with ?= @request.auth.id) || acl_type = 'public'",
  "viewRule": "owner = @request.auth.id || (acl_type = 'restricted' && shared_with ?= @request.auth.id) || acl_type = 'public'",
  "createRule": "owner = @request.auth.id",
  "updateRule": "owner = @request.auth.id",
  "deleteRule": "owner = @request.auth.id"
}
PAYLOAD
)
pb_req POST /api/collections -d "$BOARDS_PAYLOAD" > /dev/null
echo "    OK"

echo "==> Getting boards collection ID..."
BOARDS_COL=$(pb_req GET /api/collections/boards)
BOARDS_ID=$(echo "$BOARDS_COL" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    id=${BOARDS_ID}"

echo "==> Creating board_versions collection..."
VERSIONS_PAYLOAD=$(cat <<PAYLOAD
{
  "name": "board_versions",
  "type": "base",
  "fields": [
    {"name": "board", "type": "relation", "required": true, "collectionId": "${BOARDS_ID}", "maxSelect": 1, "cascadeDelete": true},
    {"name": "data", "type": "json", "required": true},
    {"name": "metadata", "type": "json", "required": true}
  ],
  "listRule": "board.owner = @request.auth.id || (board.acl_type = 'restricted' && board.shared_with ?= @request.auth.id) || board.acl_type = 'public'",
  "viewRule": "board.owner = @request.auth.id || (board.acl_type = 'restricted' && board.shared_with ?= @request.auth.id) || board.acl_type = 'public'",
  "createRule": "board.owner = @request.auth.id",
  "updateRule": "",
  "deleteRule": "board.owner = @request.auth.id"
}
PAYLOAD
)
pb_req POST /api/collections -d "$VERSIONS_PAYLOAD" > /dev/null
echo "    OK"

echo "==> PocketBase configured."

# ══════════════════════════════════════════════════════════════════════
# Pre-enroll PocketBase credentials for both demo users in OpenBao
# ══════════════════════════════════════════════════════════════════════

echo "==> Pre-enrolling PocketBase credentials for demo user..."
bao_post "/v1/secret/data/users/${DEMO_SUB}/apikeys/pocketbase" -d "{
  \"data\": {
    \"email\":    \"demo@example.com\",
    \"password\": \"${PB_USER_PASSWORD}\",
    \"url\":      \"${PB_URL}\"
  }
}"
echo "    OK"

echo "==> Pre-enrolling PocketBase credentials for demo2 user..."
bao_post "/v1/secret/data/users/${DEMO2_SUB}/apikeys/pocketbase" -d "{
  \"data\": {
    \"email\":    \"demo2@example.com\",
    \"password\": \"${PB_USER_PASSWORD}\",
    \"url\":      \"${PB_URL}\"
  }
}"
echo "    OK"

echo ""
echo "==> All services configured successfully."
echo "    PocketBase credentials for both demo users enrolled in OpenBao."
