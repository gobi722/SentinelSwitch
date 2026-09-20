# Design: External Multi-Tenant Result Delivery via Kafka

**Status:** Implemented, committed, and verified end-to-end against a running local stack (see
Verification below for what was actually confirmed and how).
**Ticket:** CREDO-ALERT-001 (follow-on)

## Context

SentinelSwitch currently returns only an immediate ACK from `SubmitTransaction` (gRPC). The actual fraud
decision is computed asynchronously and today only lands in an internal Kafka topic (`fraud_results`) and
Postgres — no external caller can retrieve it.

The goal is to make this an "independent product": any external caller (integrator/merchant platform)
should be able to call `SubmitTransaction`, get the ACK, and then **independently consume a Kafka topic**
to receive their own final fraud results (risk score, decision, everything) — asynchronously, at their own
pace, doing whatever they want with it.

Constraints confirmed for this design:
- Expected scale: tens–low hundreds of distinct external callers (not thousands+) — a per-client Kafka
  topic model is appropriate at this scale.
- Keep matching the current project convention: app services are not containerized in `docker-compose.yml`
  today (only infra is) — `result-notifier` follows the same pattern (Dockerfile + `go run` locally), not
  wired into compose in this change.

Audited starting state (verified, not assumed):
- **Zero authentication exists today.** `proto/gateway.proto` has comments claiming JWT auth, but no such
  code exists — `services/api-gateway/internal/gateway/handler.go` copies `req.MerchantId` straight from
  the client-supplied body. The only identity-like thing is an unverified `X-Client-ID`-style header used
  purely for rate-limit bucketing (`internal/ratelimit/limiter.go`).
- **Kafka has no SASL/TLS/ACLs anywhere.** Two listeners only: `INTERNAL:9093` and `EXTERNAL:9092`, both
  PLAINTEXT (`docker-compose.yml`).
- **No tenant/caller identity field exists** in `TransactionEvent` (`proto/transactions.proto`, 13 fields,
  last = `rrn=13`) or `FraudResultEvent` (`proto/fraud_results.proto`, 17 fields, last = `triggered_rules=17`).
  `merchant_id` identifies the merchant a transaction is *for*, not the API caller/integrator.
- Port `9094` is already claimed by risk-service metrics (per `docs-mine/SERVICE_RUN_GUIDE.md`) — the new
  Kafka public listener must avoid it.

## Approach

1. **API-key auth on API Gateway** (not JWT — this is server-to-server/B2B, API keys are the standard fit).
   A verified `client_id` becomes the caller's identity, completely separate from `merchant_id`.
2. **`client_id` flows through the pipeline**: added to `TransactionEvent` and mirrored into
   `FraudResultEvent`, consistent with the existing "fully enriched event, no joins" design already used
   for `fraud_results`.
3. **Per-client Kafka topic with real isolation** (`results.<client_id>`), populated by a new small service,
   **not** by exposing the internal `fraud_results` topic directly (shared-topic + client-side filtering
   would let any caller read everyone's data — not acceptable). New external-facing SASL-authenticated Kafka
   listener with ACLs restricts each client to only their own topic.
4. **Scripted provisioning**, not a self-service admin API — out of scope at this stage.

Auth must **fail closed** (opposite of the existing idempotency-store pattern, which intentionally fails
open) — a Redis/Postgres outage should return `UNAVAILABLE`, never silently admit unauthenticated traffic.

---

## A. Proto changes

- **`proto/transactions.proto`**: add `string client_id = 14;` to `TransactionEvent` (comment: verified
  integrator identity, set server-side only, never present in the inbound `TransactionRequest`).
- **`proto/fraud_results.proto`**: add `string client_id = 18;` to `FraudResultEvent`, mirrored from
  `TransactionEvent.client_id`.
- **`proto/gateway.proto`**: fix the misleading header/`UNAUTHENTICATED` comments to describe API-key auth
  via `x-api-key` metadata instead of the never-implemented JWT claim.
- Re-run `buf generate` after these edits — regenerates `services/proto-gen/transactions/v1/*.pb.go` and
  `services/proto-gen/fraud/v1/*.pb.go`; all Go services depending on these must rebuild.

## B. API Gateway — new auth subsystem

New package `services/api-gateway/internal/auth/`:
- `hash.go` — `Hash(apiKey string) string`, plain SHA-256 hex (API keys are high-entropy tokens, not
  passwords — same primitive already used in `internal/hashing/pan.go`; bcrypt's slowness is the wrong
  trade-off here).
- `store.go` — `Store` over a `pgxpool.Pool` (same driver persistence-svc already uses):
  `Lookup(ctx, apiKeyHash) (clientID, status string, err error)`.
- `cache.go` — Redis-backed read-through cache in front of `Store`, TTL-bound, **fails closed**
  (`codes.Unavailable` on any backing-store error — never allow-through).
- `context.go` — `WithClientID`/`ClientIDFromContext` helpers.
- `interceptor.go` — `UnaryServerInterceptor`: skips `/grpc.health.v1.Health/*`; reads `x-api-key` from
  gRPC metadata (missing → `Unauthenticated`); hashes + looks up via cache (not found/inactive →
  `Unauthenticated`/`PermissionDenied`); injects verified `client_id` into context **and** rewrites the
  incoming `x-client-id` metadata key to the verified value, so the existing rate limiter
  (`internal/ratelimit/limiter.go`) automatically becomes trustworthy with zero changes to it.

Modified files:
- `internal/gateway/handler.go`: in `SubmitTransaction`, after idempotency check, set
  `event.ClientId = clientID` (from `auth.ClientIDFromContext(ctx)`) when building `TransactionEvent`.
- `cmd/main.go`: wire a Postgres pool + Redis client + `auth.NewCache(...)`, add the interceptor into the
  existing `grpc_middleware.ChainUnaryServer(...)` (after zap/recovery, before the handler).
- `internal/config/config.go`: new `Auth` struct (header name, cache DB index, cache TTL) + `Postgres`
  block, same shape as `persistence-svc/internal/config/config.go`'s `PostgresConfig`/`PoolConfig`.
- `config/api-gateway.yaml`: new `auth:` and `postgres:` sections (`${VAR:-default}` style, matching
  `persistence-svc.yaml`).
- `config/redis.yaml`: add `api_key_cache: 3` DB assignment (existing: velocity=0, idempotency=1,
  circuit_breaker=2), `volatile-lru` eviction (cache-miss on eviction just costs one DB hit).
- `go.mod`: add `github.com/jackc/pgx/v5` (pin to persistence-svc's existing version).

## C. Fraud Engine — minimal passthrough

- `internal/pipeline/processor.go`: one line when constructing `fraudpb.FraudResultEvent` —
  `ClientId: txn.ClientId,`. No other changes.

## D. New Postgres migration

`db/migrations/003_create_api_clients.sql`:
```sql
CREATE TABLE api_clients (
    client_id      VARCHAR(64)  PRIMARY KEY,
    api_key_hash   CHAR(64)     NOT NULL UNIQUE,
    api_key_prefix VARCHAR(12),                    -- display/support only, never the full key
    name           VARCHAR(100) NOT NULL,
    status         VARCHAR(10)  NOT NULL DEFAULT 'active', -- active | suspended
    created_at     TIMESTAMP    NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMP    NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_api_clients_status ON api_clients (status);
```
Note: `docker-entrypoint-initdb.d` only runs against a fresh Postgres volume — same caveat migration 002
already had. Apply manually via `psql -f` against the running container.

## E. New service: `result-notifier`

Mirrors `persistence-svc`'s shape most closely (single input topic, manual commit, DLQ-on-failure):

```
services/result-notifier/
  cmd/main.go
  go.mod                          # module github.com/sentinelswitch/result-notifier; replace proto => ../proto-gen
  Dockerfile                      # same scratch multi-stage pattern as the other 4
  internal/config/config.go       # same expandEnv/applyDefaults pattern as persistence-svc's config.go
  internal/router/router.go       # ResolveTopic(clientID) — validates against ^[a-zA-Z0-9._-]{1,200}$, prefixes with "results."
  internal/kafka/dlq.go           # adapted from persistence-svc/internal/kafka/dlq.go
  internal/pipeline/processor.go  # consume fraud_results (group: result-notifier-cg) -> decode -> router.ResolveTopic -> publish to results.<client_id> -> manual commit only after success or DLQ
```

- Consumes the existing internal `fraud_results` topic over the trusted `INTERNAL:9093` listener (no SASL
  needed — it's inside the trust boundary), republishes to `results.<client_id>` on the new `PUBLIC`
  listener.
- Unroutable events (empty/invalid `client_id`, unprovisioned topic) go to a new `results_unrouted_dlq`
  topic, same header pattern as `persistence-svc`'s DLQ (`dlq-original-topic`, `dlq-failure-reason`,
  `dlq-retry-count`, `dlq-failed-at`).
- Commit-after-DLQ-or-success (like persistence-svc), not always-commit (like fraud-engine) — dropping a
  paying integrator's result silently is worse than a bounded retry.
- New metrics: `sentinel_result_notifier_messages_routed_total`, `sentinel_result_notifier_unrouted_total`
  — plain counters, **not** labeled by `client_id` (unbounded cardinality risk).
- `config/result-notifier.yaml` — new file, same conventions as the other 4 config files.
- Ports: metrics `9098`, health `8085` (next free ports per `docs-mine/SERVICE_RUN_GUIDE.md`'s existing
  allocation).

## F. `docker-compose.yml` — Kafka only

Add a third listener, scoped so it touches nothing existing:
```yaml
KAFKA_LISTENERS: INTERNAL://0.0.0.0:9093,EXTERNAL://0.0.0.0:9092,PUBLIC://0.0.0.0:9096
KAFKA_ADVERTISED_LISTENERS: INTERNAL://kafka:9093,EXTERNAL://localhost:9092,PUBLIC://localhost:9096
KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: INTERNAL:PLAINTEXT,EXTERNAL:PLAINTEXT,PUBLIC:SASL_PLAINTEXT
KAFKA_SASL_ENABLED_MECHANISMS: SCRAM-SHA-256
KAFKA_LISTENER_NAME_PUBLIC_SASL_ENABLED_MECHANISMS: SCRAM-SHA-256
KAFKA_LISTENER_NAME_PUBLIC_SCRAM-SHA-256_SASL_JAAS_CONFIG: "org.apache.kafka.common.security.scram.ScramLoginModule required;"
KAFKA_AUTHORIZER_CLASS_NAME: kafka.security.authorizer.AclAuthorizer
KAFKA_SUPER_USERS: User:ANONYMOUS
```
plus `"9096:9096"` in `ports:`. `User:ANONYMOUS` as super-user keeps `INTERNAL`/`EXTERNAL` listeners
behaving exactly as today (all 4 existing consumer groups are anonymous) — 100% of new ACL enforcement is
confined to the new `PUBLIC` listener. No other compose changes (per the call to keep app services
un-containerized for now).

**SASL_PLAINTEXT, not SASL_SSL**: protects credentials, not payload confidentiality. Correct trade-off for
dev/portfolio scope — TLS is a named production follow-up, not silently skipped.

**Schema Registry stays internal-only** — do not expose it externally. External clients get the `.proto`
contract distributed out-of-band; they only need the 5-byte Confluent frame's schema-id to strip the
header, not registry access at runtime.

## G. `config/kafka-topics.yaml`

- New fixed topic `results_unrouted_dlq` (3 partitions, 14-day retention — same shape as `transaction_dlq`).
- New consumer group `result-notifier-cg` (modeled on `fraud-engine-cg`: low-latency, manual commit).
- Documentation block (not a fixed entry, since these topics are created dynamically) describing the
  `results.<client_id>` naming convention and pointing at the provisioning script; local-dev provisioning
  uses 1 partition / RF 1 (matches the single-broker cluster, same divergence the existing quick-start
  commands already make from the file's stated RF 3).

## H. Provisioning script

`scripts/provision-client.sh <client_id> <display_name>` (bash):
1. Generate a random API key (`openssl rand -hex 32`), SHA-256 it, insert into `api_clients` via `psql`.
2. Generate a random SCRAM password; `docker compose exec kafka kafka-configs --bootstrap-server kafka:9093
   --alter --add-config 'SCRAM-SHA-256=[password=...]' --entity-type users --entity-name <client_id>`.
3. `kafka-topics --create --topic results.<client_id> --partitions 1 --replication-factor 1`.
4. `kafka-acls --add --allow-principal User:<client_id> --operation Read --topic results.<client_id>` and
   a group ACL (`--group <client_id> --resource-pattern-type prefixed`) — Kafka requires both topic *and*
   consumer-group ACLs for external reads to work; caller-chosen group IDs must be prefixed with
   `<client_id>.` by convention.
5. Print the plaintext API key + SCRAM password once. Nothing is stored in plaintext after this.

Documented as a runbook step, not a CRUD service — matches the confirmed scope.

---

## Verification

1. **Build**: `go build ./...` in each modified/new service directory (api-gateway, fraud-engine,
   result-notifier) — must compile cleanly after `buf generate`.
   ✅ **Confirmed.** All three run cleanly via `go run ./cmd` against the local stack; no build errors.
2. **Provision a test client**: run `scripts/provision-client.sh test-client "Test Integrator"`, capture the
   printed API key + SCRAM password.
   ✅ **Confirmed.** `test-client` provisioned — `api_clients` row, SCRAM credential, `results.test-client`
   topic, and both ACLs (topic read + `test-client.`-prefixed group read) all created successfully.
3. **Submit a transaction** via `grpcurl` with `x-api-key: <key>` metadata header set — confirm ACK.
   Submit once *without* the header — confirm `Unauthenticated`.
   ✅ **Confirmed.** Multiple `SubmitTransaction` calls with a valid `x-api-key` returned `PENDING` + `txn_id`;
   the same call with the header omitted returned `Unauthenticated: missing x-api-key`.
4. **Confirm client_id propagation**: check Fraud Engine logs / the `fraud_results` topic payload has the
   correct `client_id`.
   ✅ **Confirmed.** Decoded `FraudResultEvent` payloads (via `scripts/demo/decode-results`) show
   `client_id: "test-client"` correctly mirrored through on every submitted transaction.
5. **Confirm external delivery + isolation**: consume `results.test-client` using the SCRAM credentials on
   `localhost:9096` — the result should appear. Attempt to consume a *different* client's topic with
   `test-client`'s credentials — should be denied by the ACL.
   ✅ **Confirmed.** `results.test-client` consumed successfully with `test-client`'s SCRAM credentials
   (decoded results matched what was submitted). Attempting to read `results.some-other-client` with the
   same credentials failed with `TOPIC_AUTHORIZATION_FAILED`, as expected.
6. **Confirm fail-closed auth**: stop Redis and Postgres, retry a `SubmitTransaction` call — expect
   `UNAVAILABLE`, not a silently-accepted request.
   ✅ **Confirmed, via a real-world variant.** A misconfigured `POSTGRES_PASSWORD` (auth failure connecting
   to Postgres, logged as `password authentication failed for user "sentinel"`) correctly produced
   `Unavailable: auth temporarily unavailable` rather than admitting the request — i.e. the backing-store
   failure path fails closed as designed. The literal "stop the Redis/Postgres containers mid-flight" variant
   has not been separately exercised, though it exercises the same code path.
7. **Confirm unrouted DLQ**: manually publish a `fraud_results` message with an empty/garbage `client_id`
   directly to Kafka — confirm it lands in `results_unrouted_dlq`, not silently dropped.
   ✅ **Confirmed.** Published a `FraudResultEvent` with `client_id: ""` directly to `fraud_results` — it
   landed in `results_unrouted_dlq` with the full diagnostic header set (`dlq-original-topic: fraud_results`,
   `dlq-original-partition`, `dlq-original-offset`, `dlq-failure-reason: unroutable_client_id`,
   `dlq-retry-count: 0`, `dlq-failed-at`), same key as the original message, and the untouched original
   payload as the value — matching the design exactly.
   Note: `results_unrouted_dlq` did not already exist on the local broker (auto-create is disabled) — it had
   to be created manually before this check could run. Without it, `result-notifier` would fail to DLQ (and
   therefore never commit, retrying indefinitely) the first unroutable message it ever saw on a fresh
   environment. This was a real gap in local-dev setup, now fixed: the topic-creation step has been added to
   `docs/INFRASTRUCTURE_SETUP.md` §4.1 alongside `transaction_dlq`.

All verified checks were run manually against the local `docker-compose` stack plus locally-run Go
services (not yet exercised in CI or a deployed environment). A live client-facing demo of the delivery
path (submit via Postman/grpcurl → decision arrives on the client's own `results.<client_id>` topic) was
also built on top of this: see `scripts/demo/live-dashboard`.

---

## Risks / open trade-offs (for future reference)

1. **Topic-per-tenant doesn't scale indefinitely.** Fine at the confirmed scale (tens–low hundreds); would
   need a different mechanism (e.g. an authenticated streaming gateway with server-side filtering) if the
   tenant count ever grows into the thousands.
2. **SASL_PLAINTEXT ships fraud decisions unencrypted** on the new listener — only credentials are
   protected. TLS (`SASL_SSL`) is the production-grade follow-up.
3. **`api_clients` migration won't auto-apply** to an already-initialized Postgres volume — manual `psql -f`
   needed, same as migration 002.
4. Naming: `client_id` sits next to Kafka's unrelated `client_id` producer setting already used throughout
   the config files — a readability nit only, not a technical conflict. `integrator_id` would be less
   ambiguous if renamed before `buf generate` is run.
