#!/usr/bin/env sh
#
# Configure OpenBao for the hello-postgres example:
#
#   1. JWT auth method, backed by Dex (for user login).
#   2. AppRole auth method (for blockyard's own admin token).
#   3. Database secrets engine + postgres connection (vault owns the
#      per-user `user_<entity-id>` passwords and rotates them).
#   4. blockyard-server policy (scoped: can create/update user_* DB
#      static roles; cannot read per-user creds; cannot touch vault's
#      identity side).
#   5. blockyard-user-template policy (templated per-user scope for
#      reading one's own DB creds — ACL enforced at the vault layer).
#   6. blockyard-user JWT role, with the per-user template attached via
#      token_policies so every login carries it.
#
# The security invariant this script sets up: blockyard's token can
# define `user_*` DB static roles but cannot mint their creds; each
# user's token can only read its own creds. No single token holds the
# union. This is the "Blockyard out of the runtime trust chain"
# property from docs/design/board-storage.md, made concrete — operators
# are expected to adapt this script, keeping these capability
# boundaries intact.
#
# Runs as a one-shot init container after OpenBao, Dex, and PostgreSQL
# are healthy.

set -eux

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
BAO_TOKEN="${BAO_TOKEN:-root-dev-token}"
DEX_ISSUER="${DEX_ISSUER:-http://localhost:5556}"
DEX_INTERNAL="${DEX_INTERNAL:-${DEX_ISSUER}}"
APPROLE_SECRET_ID="${APPROLE_SECRET_ID:-dev-secret-id-for-local-use-only}"
PG_HOST="${PG_HOST:-postgres}"
PG_PORT="${PG_PORT:-5432}"
PG_DB="${PG_DB:-blockyard}"
PG_ADMIN_USER="${PG_ADMIN_USER:-vault_db_admin}"
PG_ADMIN_PASSWORD="${PG_ADMIN_PASSWORD:-dev-password}"

bao_header="-H X-Vault-Token:${BAO_TOKEN} -H Content-Type:application/json"

bao_post() {
  path="$1"; shift
  # shellcheck disable=SC2086
  curl -f --show-error $bao_header -X POST "${BAO_ADDR}${path}" "$@"
}

# ══════════════════════════════════════════════════════════════════════
# Wait for upstreams
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

# ══════════════════════════════════════════════════════════════════════
# 1. JWT auth (Dex-backed)
# ══════════════════════════════════════════════════════════════════════
#
# Each login mints a vault token scoped by the per-user template
# policy below. blockyard never writes anything on the identity side;
# the OIDC auth method is responsible for entity creation at first
# login, and the entity ID becomes the PG role name blockyard
# provisions.

echo "==> Enabling JWT auth method..."
bao_post /v1/sys/auth/jwt -d '{"type":"jwt"}' || true

echo "==> Configuring JWT auth with Dex..."
bao_post /v1/auth/jwt/config -d "{
  \"jwks_url\":      \"${DEX_INTERNAL}/keys\",
  \"bound_issuer\":  \"${DEX_ISSUER}\",
  \"default_role\":  \"blockyard-user\"
}"

# ══════════════════════════════════════════════════════════════════════
# 2. AppRole (blockyard's own token)
# ══════════════════════════════════════════════════════════════════════

echo "==> Enabling AppRole auth method..."
bao_post /v1/sys/auth/approle -d '{"type":"approle"}' || true

# ══════════════════════════════════════════════════════════════════════
# 3. Database secrets engine + postgres connection
# ══════════════════════════════════════════════════════════════════════
#
# The mount name below matches database.vault_mount in blockyard.toml
# (default "database"). The connection name matches
# database.vault_db_connection — blockyard uses it as `db_name` when
# it POSTs static-role definitions. vault_db_admin owns the per-user
# passwords (rotated by vault); blockyard never sees them.

echo "==> Enabling database secrets engine at database/..."
bao_post /v1/sys/mounts/database -d '{"type":"database"}' || true

echo "==> Registering the 'blockyard' postgres connection..."
bao_post /v1/database/config/blockyard -d "{
  \"plugin_name\":    \"postgresql-database-plugin\",
  \"connection_url\": \"postgresql://{{username}}:{{password}}@${PG_HOST}:${PG_PORT}/${PG_DB}?sslmode=disable\",
  \"username\":       \"${PG_ADMIN_USER}\",
  \"password\":       \"${PG_ADMIN_PASSWORD}\",
  \"allowed_roles\":  [\"user_*\"]
}"

# ══════════════════════════════════════════════════════════════════════
# 4. blockyard-server policy (token held by blockyard itself)
# ══════════════════════════════════════════════════════════════════════
#
# Capabilities granted:
#   - Create/update/delete/read user_* DB static-role *definitions*.
#     Blockyard defines them at first login; it does not hold a path
#     that reads their credentials.
#   - Read own admin creds at database/static-creds/blockyard_app.
#   - POST identity/lookup/entity to resolve an OIDC alias to an
#     entity ID (the PG role name blockyard provisions).
#   - Read sys/auth once at startup to resolve the OIDC mount accessor.
#   - The pre-existing KV + AppRole paths blockyard needs for its
#     own operation (user-enrolled API keys, session secret, etc.).
#
# Explicitly NOT granted (these would break the security invariant):
#   - database/static-creds/user_*      (minting user DB creds)
#   - identity/entity/id/*              (writing vault identity data)
#   - sys/policies/acl/*                (policy writes at runtime)

echo "==> Creating blockyard-server policy..."
bao_post /v1/sys/policy/blockyard-server -d '{
  "policy": "path \"database/static-roles/user_*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\"]\n}\npath \"database/static-creds/blockyard_app\" {\n  capabilities = [\"read\"]\n}\npath \"identity/lookup/entity\" {\n  capabilities = [\"update\"]\n}\npath \"sys/auth\" {\n  capabilities = [\"read\"]\n}\npath \"sys/mounts\" {\n  capabilities = [\"read\"]\n}\npath \"sys/policies/acl/*\" {\n  capabilities = [\"read\"]\n}\npath \"auth/jwt/role/blockyard-user\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/renew-self\" {\n  capabilities = [\"update\"]\n}\npath \"secret/data/blockyard/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\"]\n}\npath \"secret/metadata/blockyard/*\" {\n  capabilities = [\"read\", \"list\"]\n}\npath \"secret/data/users/*\" {\n  capabilities = [\"create\", \"update\"]\n}\npath \"secret/metadata/users/*\" {\n  capabilities = [\"read\"]\n}"
}'

echo "==> Creating blockyard-server AppRole role..."
bao_post /v1/auth/approle/role/blockyard-server -d '{
  "token_policies":  ["blockyard-server"],
  "token_ttl":       "1h",
  "token_max_ttl":   "0"
}'

echo "==> Setting custom role_id for dev..."
bao_post /v1/auth/approle/role/blockyard-server/role-id -d '{"role_id": "blockyard-server"}' || true

echo "==> Setting custom secret_id for dev..."
bao_post /v1/auth/approle/role/blockyard-server/custom-secret-id -d "{
  \"secret_id\": \"${APPROLE_SECRET_ID}\"
}" || true

# ══════════════════════════════════════════════════════════════════════
# 5. blockyard-user-template — per-user templated policy
# ══════════════════════════════════════════════════════════════════════
#
# Attached to every user token (via token_policies on the JWT role
# below). The {{identity.entity.id}} expression resolves from the
# token's own auth context to the caller's entity UUID — i.e. the
# same value blockyard used when it derived the PG role name. ACL
# enforcement is server-side: even if R constructs a mismatched path,
# the policy check rejects it.

echo "==> Creating blockyard-user-template policy..."
bao_post /v1/sys/policy/blockyard-user-template -d '{
  "policy": "path \"database/static-creds/user_{{identity.entity.id}}\" {\n  capabilities = [\"read\"]\n}\npath \"secret/data/users/{{identity.entity.id}}/*\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/lookup-self\" {\n  capabilities = [\"read\"]\n}\npath \"auth/token/renew-self\" {\n  capabilities = [\"update\"]\n}"
}'

# ══════════════════════════════════════════════════════════════════════
# 6. blockyard-user JWT role — attaches the templated policy to every
# token issued via OIDC login.
# ══════════════════════════════════════════════════════════════════════
#
# user_claim = "sub" (default). No claim_mappings needed: blockyard
# looks up the entity by alias at provisioning time, so whichever
# alias name the JWT auth method records works here.

echo "==> Creating blockyard-user JWT role..."
bao_post /v1/auth/jwt/role/blockyard-user -d '{
  "role_type":       "jwt",
  "bound_audiences": ["blockyard"],
  "user_claim":      "sub",
  "token_policies":  ["blockyard-user-template"],
  "token_ttl":       "1h"
}'

echo ""
echo "==> All services configured successfully."
echo "    - Blockyard can define user_* DB roles but cannot read their creds."
echo "    - Each user's token can only read its own static-creds/user_<entity-id>."
echo "    - Rotation of per-user passwords is owned by vault, not blockyard."
