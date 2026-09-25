#!/usr/bin/env bash
# run.sh — load-test API Gateway's SubmitTransaction with ghz
# (https://ghz.sh), a gRPC benchmarking tool.
#
# Every request gets a unique idempotency_key and card_hash (via ghz's
# {{.UUID}} template function in transaction.tmpl.json) so none are rejected
# as duplicates or pile onto one card's velocity counters.
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
#   scripts/loadtest/run.sh <api_key> 200 60s           # 200 concurrent, 60s
#   scripts/loadtest/run.sh <api_key> 200 60s host:port # against a non-default target
#
# Requires: ghz (go install github.com/bojand/ghz/cmd/ghz@latest)
set -euo pipefail

API_KEY="${1:-}"
CONCURRENCY="${2:-50}"
DURATION="${3:-30s}"
TARGET="${4:-localhost:50051}"

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

echo "==> Target: $TARGET | concurrency: $CONCURRENCY | duration: $DURATION"
echo "==> HTML report will be written to: $HTML_REPORT"
echo

ghz --insecure \
  --proto "$REPO_ROOT/proto/gateway.proto" \
  --import-paths "$REPO_ROOT/proto" \
  --call sentinel.gateway.v1.GatewayService.SubmitTransaction \
  --data-file "$SCRIPT_DIR/transaction.tmpl.json" \
  --metadata "{\"x-api-key\":\"$API_KEY\"}" \
  --concurrency "$CONCURRENCY" \
  --duration "$DURATION" \
  --format html \
  --output "$HTML_REPORT" \
  "$TARGET"

echo
echo "==> Done. Open $HTML_REPORT for the full report (RPS, latency histogram, status breakdown)."
