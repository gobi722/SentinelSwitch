#!/usr/bin/env bash
# provision-client.sh — onboard a new external API caller/integrator.
#
# Creates everything a new client needs to use SentinelSwitch as an
# independent product:
#   1. An API key (for gRPC SubmitTransaction auth) — stored hashed in
#      Postgres, api_clients table.
#   2. A Kafka SCRAM-SHA-256 credential (for reading their own results).
#   3. A dedicated Kafka topic: results.<client_id>
#   4. ACLs restricting that credential to ONLY that topic + a
#      "<client_id>." prefixed consumer-group namespace.
#
# See docs/MULTI_TENANT_RESULT_DELIVERY.md for the full design.
#
# Usage:
#   scripts/provision-client.sh <client_id> <display_name>
#
# Requires: docker compose stack running (postgres, kafka), openssl, psql
# reachable via `docker exec sentinel-postgres`.
set -euo pipefail

CLIENT_ID="${1:-}"
DISPLAY_NAME="${2:-}"

if [[ -z "$CLIENT_ID" || -z "$DISPLAY_NAME" ]]; then
  echo "Usage: $0 <client_id> <display_name>" >&2
  exit 1
fi

# Must match the router's validation (services/result-notifier/internal/router)
# and be a legal Kafka topic-name component, since it's interpolated directly
# into results.<client_id>.
if ! [[ "$CLIENT_ID" =~ ^[a-zA-Z0-9._-]{1,200}$ ]]; then
  echo "error: client_id must match ^[a-zA-Z0-9._-]{1,200}\$ (got: $CLIENT_ID)" >&2
  exit 1
fi

TOPIC="results.${CLIENT_ID}"
GROUP_PREFIX="${CLIENT_ID}."

echo "==> Provisioning client '${CLIENT_ID}' (${DISPLAY_NAME})"

# ---------------------------------------------------------------------------
# 1. API key (Postgres — services/api-gateway/internal/auth)
# ---------------------------------------------------------------------------
API_KEY="$(openssl rand -hex 32)"
API_KEY_HASH="$(printf '%s' "$API_KEY" | openssl dgst -sha256 -hex | awk '{print $NF}')"
API_KEY_PREFIX="${API_KEY:0:12}"

echo "==> Inserting into api_clients (Postgres)"
docker exec -i sentinel-postgres psql -U sentinel -d sentinelswitch -v ON_ERROR_STOP=1 <<SQL
INSERT INTO api_clients (client_id, api_key_hash, api_key_prefix, name, status)
VALUES ('${CLIENT_ID}', '${API_KEY_HASH}', '${API_KEY_PREFIX}', '${DISPLAY_NAME}', 'active');
SQL

# ---------------------------------------------------------------------------
# 2. Kafka SCRAM-SHA-256 credential
# ---------------------------------------------------------------------------
SCRAM_PASSWORD="$(openssl rand -hex 24)"

echo "==> Creating SCRAM-SHA-256 credential for principal User:${CLIENT_ID}"
docker compose exec -T kafka kafka-configs \
  --bootstrap-server kafka:9093 \
  --alter \
  --add-config "SCRAM-SHA-256=[password=${SCRAM_PASSWORD}]" \
  --entity-type users \
  --entity-name "${CLIENT_ID}"

# ---------------------------------------------------------------------------
# 3. Dedicated results topic
# ---------------------------------------------------------------------------
echo "==> Creating topic ${TOPIC}"
docker compose exec -T kafka kafka-topics \
  --bootstrap-server kafka:9093 \
  --create \
  --if-not-exists \
  --topic "${TOPIC}" \
  --partitions 1 \
  --replication-factor 1

# ---------------------------------------------------------------------------
# 4. ACLs — topic read + prefixed consumer-group read
# ---------------------------------------------------------------------------
echo "==> Granting ACLs to User:${CLIENT_ID}"
docker compose exec -T kafka kafka-acls \
  --bootstrap-server kafka:9093 \
  --add \
  --allow-principal "User:${CLIENT_ID}" \
  --operation Read \
  --topic "${TOPIC}"

docker compose exec -T kafka kafka-acls \
  --bootstrap-server kafka:9093 \
  --add \
  --allow-principal "User:${CLIENT_ID}" \
  --operation Read \
  --group "${GROUP_PREFIX}" \
  --resource-pattern-type prefixed

# ---------------------------------------------------------------------------
# Done — print secrets ONCE. Nothing after this stores them in plaintext.
# ---------------------------------------------------------------------------
cat <<EOF

==> Client '${CLIENT_ID}' provisioned successfully.

  API key (use as gRPC metadata "x-api-key"):
    ${API_KEY}

  Kafka SASL/SCRAM credentials (PUBLIC listener, localhost:9096):
    username: ${CLIENT_ID}
    password: ${SCRAM_PASSWORD}
    mechanism: SCRAM-SHA-256

  Your results topic:
    ${TOPIC}

  Consumer group IDs you use MUST start with: ${GROUP_PREFIX}

Save these now — they are not stored in plaintext and will not be shown again.
EOF
