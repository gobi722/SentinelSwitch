#!/usr/bin/env bash
# run.sh — load-test API Gateway's SubmitTransaction with ghz
# (https://ghz.sh), a gRPC benchmarking tool.
#
# Every request gets a unique idempotency_key and card_hash (via ghz's
# {{.UUID}} template function in transaction.tmpl.json) so none are rejected
# as duplicates or pile onto one card's velocity counters. amount_minor is
# randomized per call (1,000-2,000,000 minor units, i.e. 1-2,000 OMR) via
# ghz's Sprig template functions — spans both sides of fraud-rules.yaml's
# HIGH_AMOUNT threshold, so this naturally produces a mix of APPROVE/REVIEW
# decisions instead of only APPROVE like a fixed low amount would.
#
# BEFORE RUNNING:
#   1. Provision a client and get an API key:
#        grpcurl -plaintext -H "x-admin-key: <ADMIN_API_KEY>" \
#          -d '{"client_id": "loadtest", "display_name": "Load Test"}' \
#          localhost:50051 sentinel.gateway.v1.AdminService/ProvisionClient
#      (or scripts/provision-client.sh loadtest "Load Test")
#   2. The rate limiter is per-client_id (config/api-gateway.yaml ->
#      rate_limiting), default 100 req/s + burst 200 — nowhere near enough
#      to demonstrate real throughput on its own. Raise it for this run
#      (repo-root .env, then `docker compose up -d --build api-gateway`):
#        RATE_LIMITING_RPS=5000
#        RATE_LIMITING_BURST=2000
#
# Usage:
#   scripts/loadtest/run.sh <api_key> [concurrency] [duration] [target]
#
# Examples:
#   scripts/loadtest/run.sh <api_key>                  # defaults: -c 50 -z 30s
#   scripts/loadtest/run.sh <api_key> 200 60s           # 200 concurrent, max speed, 60s
#   scripts/loadtest/run.sh <api_key> 200 60s host:port # against a non-default target
#
# For a SUSTAINED, RATE-LIMITED soak test (e.g. "10 TPS for 1 hour") instead of
# a max-speed burst, set RPS — concurrency here just needs to be enough workers
# to sustain that rate (low concurrency is fine for a low rate):
#   RPS=10 scripts/loadtest/run.sh <api_key> 10 3600s
# Without RPS, concurrency alone controls speed and ghz sends as fast as it can
# — e.g. `200 6000s` with no RPS cap is NOT "a gentle test for 100 minutes", it's
# "hammer at max speed (400+ req/s) for 100 minutes straight" and will rebuild
# the same backlog we just cleared.
#
# Requires: ghz (go install github.com/bojand/ghz/cmd/ghz@latest)
set -euo pipefail

API_KEY="${1:-}"
CONCURRENCY="${2:-50}"
DURATION="${3:-30s}"
TARGET="${4:-localhost:50051}"
RPS="${RPS:-0}"   # 0 = no rate limit (ghz default: send as fast as concurrency allows)

if [[ -z "$API_KEY" ]]; then
  echo "Usage: $0 <api_key> [concurrency] [duration] [target]" >&2
  exit 1
fi

if ! command -v ghz >/dev/null 2>&1; then
  echo "error: ghz not found on PATH. Install it with:" >&2
  echo "  go install github.com/bojand/ghz/cmd/ghz@latest" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
OUT_DIR="$SCRIPT_DIR/reports"
mkdir -p "$OUT_DIR"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
HTML_REPORT="$OUT_DIR/report-$TIMESTAMP.html"

RPS_LABEL="uncapped"
if [[ "$RPS" -gt 0 ]]; then RPS_LABEL="${RPS} req/s"; fi
echo "==> Target: $TARGET | concurrency: $CONCURRENCY | duration: $DURATION | rate: $RPS_LABEL"
echo "==> HTML report will be written to: $HTML_REPORT"
echo

GHZ_ARGS=(
  --insecure
  --proto "$REPO_ROOT/proto/gateway.proto"
  --import-paths "$REPO_ROOT/proto"
  --call sentinel.gateway.v1.GatewayService.SubmitTransaction
  --data-file "$SCRIPT_DIR/transaction.tmpl.json"
  --metadata "{\"x-api-key\":\"$API_KEY\"}"
  --concurrency "$CONCURRENCY"
  --duration "$DURATION"
  --format html
  --output "$HTML_REPORT"
)
if [[ "$RPS" -gt 0 ]]; then
  GHZ_ARGS+=(--rps "$RPS")
fi

ghz "${GHZ_ARGS[@]}" "$TARGET"

echo
echo "==> Done. Open $HTML_REPORT for the full report (RPS, latency histogram, status breakdown)."
