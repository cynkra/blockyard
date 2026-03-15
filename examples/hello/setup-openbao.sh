#!/usr/bin/env sh
#
# Configure OpenBao for blockyard: enable JWT auth backed by Dex,
# AppRole auth for the server, and create policies and roles.
#
# Runs as a one-shot init container after OpenBao and Dex are healthy.
#
set -eu

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
BAO_TOKEN="${BAO_TOKEN:-root-dev-token}"
DEX_ISSUER="${DEX_ISSUER:-http://dex:5556}"
APPROLE_SECRET_ID="${APPROLE_SECRET_ID:-dev-secret-id-for-local-use-only}"

header="-H X-Vault-Token:${BAO_TOKEN} -H Content-Type:application/json"

post() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -sf $header -X POST "${BAO_ADDR}${path}" "$@"
}

echo "==> Waiting for Dex JWKS..."
for i in $(seq 1 60); do
  if curl -sf "${DEX_ISSUER}/keys" > /dev/null 2>&1; then
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
post /v1/sys/auth/jwt -d '{"type":"jwt"}' || true
echo "    OK"

echo "==> Configuring JWT auth with Dex..."
post /v1/auth/jwt/config -d "{
  \"jwks_url\":      \"${DEX_ISSUER}/keys\",
  \"bound_issuer\":  \"${DEX_ISSUER}\",
  \"default_role\":  \"blockyard-user\"
}"
echo "    OK"

echo "==> Creating blockyard-user policy..."
post /v1/sys/policy/blockyard-user -d '{
  "policy": "path \"secret/data/users/{{identity.entity.aliases.auth_jwt_*.name}}/*\" {\n  capabilities = [\"read\"]\n}"
}'
echo "    OK"

echo "==> Creating blockyard-user role..."
post /v1/auth/jwt/role/blockyard-user -d '{
  "role_type":       "jwt",
  "bound_audiences": ["blockyard"],
  "user_claim":      "sub",
  "token_policies":  ["blockyard-user"],
  "token_ttl":       "1h"
}'
echo "    OK"

# ── AppRole auth for blockyard server ──

echo "==> Enabling AppRole auth method..."
post /v1/sys/auth/approle -d '{"type":"approle"}' || true
echo "    OK"

echo "==> Creating blockyard-server policy..."
post /v1/sys/policy/blockyard-server -d '{
  "policy": "path \"secret/data/blockyard/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\"]\n}\npath \"secret/metadata/blockyard/*\" {\n  capabilities = [\"read\", \"list\"]\n}\npath \"sys/auth\" {\n  capabilities = [\"read\"]\n}\npath \"sys/mounts\" {\n  capabilities = [\"read\"]\n}\npath \"sys/policies/acl/*\" {\n  capabilities = [\"read\"]\n}\npath \"auth/jwt/role/blockyard-user\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/renew-self\" {\n  capabilities = [\"update\"]\n}\npath \"secret/data/users/*\" {\n  capabilities = [\"create\", \"update\"]\n}\npath \"secret/metadata/users/*\" {\n  capabilities = [\"read\"]\n}"
}'
echo "    OK"

echo "==> Creating blockyard-server AppRole role..."
post /v1/auth/approle/role/blockyard-server -d '{
  "token_policies":  ["blockyard-server"],
  "token_ttl":       "1h",
  "token_max_ttl":   "0"
}'
echo "    OK"

echo "==> Setting custom secret_id for dev..."
post /v1/auth/approle/role/blockyard-server/custom-secret-id -d "{
  \"secret_id\": \"${APPROLE_SECRET_ID}\"
}"
echo "    OK"

echo "==> OpenBao configured successfully."
