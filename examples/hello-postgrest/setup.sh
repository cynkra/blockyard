#!/usr/bin/env sh
#
# Combined init: configure OpenBao (JWT auth, AppRole, Identity OIDC)
# and download JWKS for PostgREST.
#
# Runs as a one-shot init container after OpenBao, Dex, and PostgreSQL
# are started.
#
set -eux

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
BAO_TOKEN="${BAO_TOKEN:-root-dev-token}"
DEX_ISSUER="${DEX_ISSUER:-http://localhost:5556}"
DEX_INTERNAL="${DEX_INTERNAL:-${DEX_ISSUER}}"
APPROLE_SECRET_ID="${APPROLE_SECRET_ID:-dev-secret-id-for-local-use-only}"
DEMO_SUB="${DEMO_SUB:-Cg1kZW1vLXVzZXItMDAxEgVsb2NhbA}"
DEMO2_SUB="${DEMO2_SUB:-Cg1kZW1vLXVzZXItMDAyEgVsb2NhbA}"

bao_header="-H X-Vault-Token:${BAO_TOKEN} -H Content-Type:application/json"

bao_post() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -f --show-error $bao_header -X POST "${BAO_ADDR}${path}" "$@"
}

bao_get() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -f --show-error $bao_header "${BAO_ADDR}${path}" "$@"
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

echo "==> Waiting for OpenBao API..."
for i in $(seq 1 30); do
  if curl -sf -H "X-Vault-Token:${BAO_TOKEN}" "${BAO_ADDR}/v1/sys/health" > /dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: OpenBao API did not become ready" >&2
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

echo "==> Creating blockyard-user role (with claim_mappings for Identity OIDC)..."
bao_post /v1/auth/jwt/role/blockyard-user -d '{
  "role_type":       "jwt",
  "bound_audiences": ["blockyard"],
  "user_claim":      "sub",
  "claim_mappings":  {"sub": "keycloak_sub"},
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

echo "==> OpenBao base configured."

# ══════════════════════════════════════════════════════════════════════
# Vault Identity OIDC setup (for PostgREST JWTs)
# ══════════════════════════════════════════════════════════════════════

echo "==> Getting JWT auth accessor..."
JWT_ACCESSOR=$(bao_get /v1/sys/auth 2>/dev/null | grep -o '"jwt/":{"accessor":"[^"]*"' | grep -o '"accessor":"[^"]*"' | cut -d'"' -f4)
if [ -z "$JWT_ACCESSOR" ]; then
  echo "ERROR: Could not get JWT auth accessor" >&2
  exit 1
fi
echo "    accessor=${JWT_ACCESSOR}"

echo "==> Creating OIDC named key 'postgrest'..."
bao_post /v1/identity/oidc/key/postgrest -d '{
  "allowed_client_ids": ["*"],
  "verification_ttl":   "2h",
  "rotation_period":    "24h",
  "algorithm":          "RS256"
}'
echo "    OK"

echo "==> Creating OIDC role 'postgrest' with claims template..."
# The template emits keycloak_sub (original IdP subject from entity alias
# name) and a fixed role for PostgREST role switching.
TEMPLATE=$(printf '{"keycloak_sub": {{identity.entity.aliases.%s.name}}, "role": "blockr_user"}' "${JWT_ACCESSOR}")
TEMPLATE_B64=$(echo -n "${TEMPLATE}" | base64 | tr -d '\n')
bao_post /v1/identity/oidc/role/postgrest -d "{
  \"key\":       \"postgrest\",
  \"client_id\": \"postgrest\",
  \"ttl\":       \"1h\",
  \"template\":  \"${TEMPLATE_B64}\"
}"
echo "    OK"

echo "==> Configuring OIDC issuer..."
bao_post /v1/identity/oidc/config -d "{
  \"issuer\": \"${BAO_ADDR}\"
}"
echo "    OK"

echo "==> Updating blockyard-user policy (add OIDC token + token renewal)..."
# Build the policy with the JWT accessor for path templating.
POLICY=$(cat <<ENDPOLICY
path "secret/data/users/{{identity.entity.aliases.${JWT_ACCESSOR}.name}}/*" {
  capabilities = ["read"]
}

path "identity/oidc/token/postgrest" {
  capabilities = ["read"]
}

path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "auth/token/renew-self" {
  capabilities = ["update"]
}
ENDPOLICY
)
# Escape the policy for JSON embedding.
POLICY_JSON=$(printf '%s' "$POLICY" | awk '{printf "%s\\n", $0}')
bao_post /v1/sys/policy/blockyard-user -d "{\"policy\": \"${POLICY_JSON}\"}"
echo "    OK"

# ══════════════════════════════════════════════════════════════════════
# Download JWKS for PostgREST
# ══════════════════════════════════════════════════════════════════════

echo "==> Downloading JWKS from vault..."
# Wait for the OIDC keys endpoint to have actual keys (the key was just
# created, so it should be available immediately).
for i in $(seq 1 30); do
  JWKS=$(curl -sf "${BAO_ADDR}/v1/identity/oidc/.well-known/keys" 2>/dev/null || true)
  if [ -n "$JWKS" ] && echo "$JWKS" | grep -q '"keys"'; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "ERROR: JWKS endpoint did not return keys" >&2
    exit 1
  fi
  sleep 1
done
echo "$JWKS" > /jwks/vault-jwks.json
echo "    OK (written to /jwks/vault-jwks.json)"

echo ""
echo "==> All services configured successfully."
echo "    Vault Identity OIDC is ready for PostgREST JWT validation."
