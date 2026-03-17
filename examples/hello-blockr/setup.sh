#!/usr/bin/env sh
#
# Combined init: configure OpenBao, Garage, and pre-enroll S3 credentials
# for the demo user.
#
# Runs as a one-shot init container after OpenBao, Dex, and Garage are started.
#
set -eu

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
BAO_TOKEN="${BAO_TOKEN:-root-dev-token}"
DEX_ISSUER="${DEX_ISSUER:-http://localhost:5556}"
DEX_INTERNAL="${DEX_INTERNAL:-${DEX_ISSUER}}"
APPROLE_SECRET_ID="${APPROLE_SECRET_ID:-dev-secret-id-for-local-use-only}"
GARAGE_ADMIN="${GARAGE_ADMIN:-http://garage:3903}"
GARAGE_ADMIN_TOKEN="${GARAGE_ADMIN_TOKEN:-garage-admin-token}"
DEMO_SUB="${DEMO_SUB:-Cg1kZW1vLXVzZXItMDAxEgVsb2NhbA}"

bao_header="-H X-Vault-Token:${BAO_TOKEN} -H Content-Type:application/json"
garage_header="-H Authorization:Bearer ${GARAGE_ADMIN_TOKEN} -H Content-Type:application/json"

bao_post() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -sf $bao_header -X POST "${BAO_ADDR}${path}" "$@"
}

garage_req() {
  method="$1"; path="$2"; shift 2
  # shellcheck disable=SC2086
  curl -sf $garage_header -X "$method" "${GARAGE_ADMIN}${path}" "$@"
}

# ══════════════════════════════════════════════════════════════════════
# OpenBao setup (JWT auth + AppRole — same as hello example)
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
  "policy": "path \"secret/data/users/{{identity.entity.aliases.auth_jwt_*.name}}/*\" {\n  capabilities = [\"read\"]\n}"
}'
echo "    OK"

echo "==> Creating blockyard-user role..."
bao_post /v1/auth/jwt/role/blockyard-user -d '{
  "role_type":       "jwt",
  "bound_audiences": ["blockyard"],
  "user_claim":      "sub",
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
# Garage setup (cluster layout, access key, bucket)
# ══════════════════════════════════════════════════════════════════════

echo "==> Waiting for Garage admin API..."
for i in $(seq 1 60); do
  if curl -sf -H "Authorization: Bearer ${GARAGE_ADMIN_TOKEN}" "${GARAGE_ADMIN}/health" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "ERROR: Garage did not become ready" >&2
    exit 1
  fi
  sleep 1
done
echo "    OK"

echo "==> Getting Garage node ID..."
STATUS=$(garage_req GET /v1/status)
NODE_ID=$(echo "$STATUS" | grep -o '"node":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    node=${NODE_ID}"

echo "==> Assigning cluster layout..."
garage_req POST /v1/layout \
  -d "[{\"id\":\"${NODE_ID}\",\"zone\":\"dc1\",\"capacity\":1073741824,\"tags\":[\"dev\"]}]"
echo "    OK"

echo "==> Applying layout..."
garage_req POST /v1/layout/apply -d '{"version":1}'
echo "    OK"

echo "==> Creating S3 access key for demo user..."
KEY_RESP=$(garage_req POST /v1/key -d '{"name":"demo-user"}')
ACCESS_KEY=$(echo "$KEY_RESP" | grep -o '"accessKeyId":"[^"]*"' | cut -d'"' -f4)
SECRET_KEY=$(echo "$KEY_RESP" | grep -o '"secretAccessKey":"[^"]*"' | cut -d'"' -f4)
echo "    accessKeyId=${ACCESS_KEY}"

echo "==> Creating S3 bucket..."
BUCKET_RESP=$(garage_req POST /v1/bucket -d '{"globalAlias":"blockyard-demo"}')
BUCKET_ID=$(echo "$BUCKET_RESP" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "    bucket=blockyard-demo (id=${BUCKET_ID})"

echo "==> Granting key access to bucket..."
garage_req POST /v1/bucket/allow \
  -d "{\"bucketId\":\"${BUCKET_ID}\",\"accessKeyId\":\"${ACCESS_KEY}\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":true}}"
echo "    OK"

echo "==> Garage configured."

# ══════════════════════════════════════════════════════════════════════
# Pre-enroll S3 credentials for the demo user in OpenBao
# ══════════════════════════════════════════════════════════════════════

echo "==> Pre-enrolling S3 credentials for demo user..."
bao_post "/v1/secret/data/users/${DEMO_SUB}/apikeys/s3" -d "{
  \"data\": {
    \"access_key\": \"${ACCESS_KEY}\",
    \"secret_key\": \"${SECRET_KEY}\",
    \"bucket\": \"blockyard-demo\",
    \"endpoint\": \"http://garage:3900\",
    \"region\": \"garage\"
  }
}"
echo "    OK"

echo ""
echo "==> All services configured successfully."
echo "    S3 credentials for demo user enrolled in OpenBao."
