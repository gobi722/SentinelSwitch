#!/usr/bin/env bash
# negative-scenarios.sh — deliberately trigger every failure/edge path the
# system has, so the negative-path metrics on the Grafana overview dashboard
# (rate-limit rejections, validation errors, duplicate/idempotency hits,
# fraud REVIEW/DECLINE, circuit breaker fallback) have real numbers behind
# them instead of sitting at zero.
#
# BEFORE RUNNING:
#   Provision a client for this script specifically (don't reuse the
#   throughput load-test client — keeps the two runs' metrics easy to tell
#   apart in the dashboard's time range):
#     grpcurl -plaintext -H "x-admin-key: <ADMIN_API_KEY>" \
#       -d '{"client_id": "negtest", "display_name": "Negative Scenarios"}' \
#       localhost:50051 sentinel.gateway.v1.AdminService/ProvisionClient
#
# Usage:
#   scripts/loadtest/negative-scenarios.sh <api_key> [burst_count] [target]
#
# Arg order deliberately mirrors run.sh's <api_key> [concurrency] [duration]
# [target> — arg 2 means "how much load" in both scripts, so a run.sh-style
# invocation doesn't get its numeric arg misread as a hostname here.
#
#   burst_count — requests fired at once in scenario 5 (rate-limit burst).
#                 Must exceed RATE_LIMITING_BURST (config/api-gateway.yaml)
#                 for any RESOURCE_EXHAUSTED to actually show up. Default
#                 3000 assumes the demo override (RATE_LIMITING_BURST=2000)
#                 is in place — if you're back on the default burst=200, a
#                 few hundred is already plenty.
#
# Requires: grpcurl, ghz (both already needed for the rest of scripts/loadtest/)
set -uo pipefail  # no -e: individual scenario failures (expected!) shouldn't abort the script

API_KEY="${1:-}"
BURST_COUNT="${2:-3000}"
TARGET="${3:-localhost:50051}"

if [[ -z "$API_KEY" ]]; then
  echo "Usage: $0 <api_key> [burst_count] [target]" >&2
  exit 1
fi

if [[ ! "$BURST_COUNT" =~ ^[0-9]+$ ]]; then
  echo "error: burst_count '$BURST_COUNT' must be a positive integer." >&2
  echo "Usage: $0 <api_key> [burst_count] [target]" >&2
  exit 1
fi

if [[ -n "${3:-}" && ! "$TARGET" =~ ^[A-Za-z0-9_.-]+:[0-9]+$ ]]; then
  echo "error: target '$TARGET' doesn't look like host:port." >&2
  echo "Usage: $0 <api_key> [burst_count] [target]" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

hr() { printf '\n=== %s ===\n' "$1"; }

# ---------------------------------------------------------------------------
# 1. Auth failure — wrong x-api-key -> UNAUTHENTICATED
# ---------------------------------------------------------------------------
hr "1/5: Invalid API key (expect UNAUTHENTICATED)"
grpcurl -plaintext \
  -H "x-api-key: not-a-real-key" \
  -d '{
    "pan_last4": "1111", "card_expiry": "12/27", "card_hash": "bad-key-test",
    "amount_minor": 50000, "currency": "OMR", "merchant_id": "M00000000000123",
    "mcc": "5411", "transaction_type": "SALES", "channel": "POS",
    "terminal_id": "NEGTEST", "idempotency_key": "'"$(date +%s%N)"'-authfail"
  }' \
  "$TARGET" sentinel.gateway.v1.GatewayService/SubmitTransaction

# ---------------------------------------------------------------------------
# 2. Validation error — unsupported currency -> INVALID_ARGUMENT
# ---------------------------------------------------------------------------
hr "2/5: Unsupported currency (expect INVALID_ARGUMENT)"
grpcurl -plaintext \
  -H "x-api-key: $API_KEY" \
  -d '{
    "pan_last4": "1111", "card_expiry": "12/27", "card_hash": "val-fail-test",
    "amount_minor": 50000, "currency": "ZZZ", "merchant_id": "M00000000000123",
    "mcc": "5411", "transaction_type": "SALES", "channel": "POS",
    "terminal_id": "NEGTEST", "idempotency_key": "'"$(date +%s%N)"'-valfail"
  }' \
  "$TARGET" sentinel.gateway.v1.GatewayService/SubmitTransaction

# ---------------------------------------------------------------------------
# 3. Duplicate idempotency key -> ALREADY_EXISTS on the second call
# ---------------------------------------------------------------------------
hr "3/5: Duplicate idempotency_key (2nd call expect ALREADY_EXISTS)"
DUP_KEY="dup-test-$(date +%s%N)"
REQ='{
  "pan_last4": "1111", "card_expiry": "12/27", "card_hash": "dup-test",
  "amount_minor": 50000, "currency": "OMR", "merchant_id": "M00000000000123",
  "mcc": "5411", "transaction_type": "SALES", "channel": "POS",
  "terminal_id": "NEGTEST", "idempotency_key": "'"$DUP_KEY"'"
}'
echo "-- first call (should succeed, PENDING) --"
grpcurl -plaintext -H "x-api-key: $API_KEY" -d "$REQ" "$TARGET" sentinel.gateway.v1.GatewayService/SubmitTransaction
echo "-- second call, same idempotency_key (should be ALREADY_EXISTS) --"
grpcurl -plaintext -H "x-api-key: $API_KEY" -d "$REQ" "$TARGET" sentinel.gateway.v1.GatewayService/SubmitTransaction

# ---------------------------------------------------------------------------
# 4. Fraud rule triggers -> REVIEW / DECLINE instead of APPROVE
#    High amount + risky MCC + foreign currency + repeated card (velocity)
#    all at once, so this reliably lands well above the DECLINE threshold.
# ---------------------------------------------------------------------------
hr "4/5: High-risk transactions (expect REVIEW/DECLINE, not APPROVE)"
RISKY_CARD="risky-card-$(date +%s%N)"
for i in $(seq 1 6); do
  grpcurl -plaintext \
    -H "x-api-key: $API_KEY" \
    -d '{
      "pan_last4": "9999", "card_expiry": "12/27", "card_hash": "'"$RISKY_CARD"'",
      "amount_minor": 900000000, "currency": "OMR", "merchant_id": "M00000000000456",
      "mcc": "7995", "transaction_type": "SALES", "channel": "PG",
      "terminal_id": "NEGTEST", "idempotency_key": "'"$(date +%s%N)"'-risky-'"$i"'"
    }' \
    "$TARGET" sentinel.gateway.v1.GatewayService/SubmitTransaction >/dev/null
  echo "  risky txn $i/6 submitted (same card_hash -> velocity rules stack)"
done
echo "Check GetTransactionStatus on these in a few seconds, or watch the Grafana pie chart —"
echo "MCC 7995 (gambling) + large amount + repeated card should trigger REVIEW/DECLINE."

# ---------------------------------------------------------------------------
# 5. Rate limit burst -> RESOURCE_EXHAUSTED for the overflow
#    Fires burst_count requests at once — must exceed RATE_LIMITING_BURST
#    to actually trigger any RESOURCE_EXHAUSTED.
# ---------------------------------------------------------------------------
hr "5/5: Rate limit burst of $BURST_COUNT requests (expect some RESOURCE_EXHAUSTED)"
if command -v ghz >/dev/null 2>&1; then
  ghz --insecure \
    --proto "$REPO_ROOT/proto/gateway.proto" \
    --import-paths "$REPO_ROOT/proto" \
    --call sentinel.gateway.v1.GatewayService.SubmitTransaction \
    --data-file "$SCRIPT_DIR/transaction.tmpl.json" \
    --metadata "{\"x-api-key\":\"$API_KEY\"}" \
    --concurrency "$BURST_COUNT" \
    --total "$BURST_COUNT" \
    "$TARGET" 2>&1 | grep -E "Status|RESOURCE_EXHAUSTED|Count|Error"
else
  echo "ghz not found — skipping burst scenario. Install: go install github.com/bojand/ghz/cmd/ghz@latest"
fi

# ---------------------------------------------------------------------------
# Manual scenarios (need a container stop/start — not automated here on
# purpose, since disrupting a running dependency is a deliberate action)
# ---------------------------------------------------------------------------
cat <<'EOF'

=== Manual scenarios (not automated) ===

A) Circuit breaker trip (populates sentinel_cb_* metrics + Fraud Engine
   falls back to REVIEW instead of calling Risk Service):
     docker compose stop risk-service
     # then run this script again (or run.sh) to send traffic —
     # after 5 consecutive failed Risk Service calls the circuit opens
     docker compose start risk-service
     # after ~15s (open_duration_seconds in config/fraud-rules.yaml) it
     # probes HALF_OPEN and recovers on 2 consecutive successes

B) DLQ trip (populates sentinel_persistence_upserts_total{status="dlq"}):
     docker compose stop postgres
     # send a few transactions — Persistence Service retries 3x then DLQs
     docker compose start postgres

Re-open the Grafana overview dashboard after either — the new panels for
circuit breaker state/fallback and persistence status will show it.
EOF
