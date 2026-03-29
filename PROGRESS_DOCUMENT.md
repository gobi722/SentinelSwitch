# SentinelSwitch — Complete Build Progress Document
**Ticket:** CREDO-ALERT-001
**Date:** 2025
**Status:** Phase 2 Complete — Fraud Engine · Risk Service · Persistence Service

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Architecture Diagram](#2-architecture-diagram)
3. [End-to-End Transaction Flow](#3-end-to-end-transaction-flow)
4. [Failure Scenarios](#4-failure-scenarios)
5. [All Files Built](#5-all-files-built)
6. [Protobuf Contracts (proto/)](#6-protobuf-contracts-proto)
7. [Configuration Files (config/)](#7-configuration-files-config)
8. [Database Layer (db/)](#8-database-layer-db)
9. [API Gateway Service (services/api-gateway/)](#9-api-gateway-service-servicesapi-gateway)
10. [Fraud Engine Service (services/fraud-engine/)](#10-fraud-engine-service-servicesfraud-engine)
11. [Risk Service (services/risk-service/)](#11-risk-service-servicesrisk-service)
12. [Persistence Service (services/persistence-svc/)](#12-persistence-service-servicespersistence-svc)
13. [Proto Code Generation (buf)](#13-proto-code-generation-buf)
14. [Key Design Decisions](#14-key-design-decisions)
15. [What Remains to Build](#15-what-remains-to-build)

---

## 1. System Overview

**SentinelSwitch** is a real-time fraud detection platform for payment transactions.

- A client (POS terminal or payment page) submits a transaction.
- The system accepts it immediately with a non-blocking ACK.
- Fraud analysis runs asynchronously through a Kafka-driven pipeline.
- The final decision is stored in PostgreSQL.
- Every service exposes Prometheus metrics → Grafana dashboards.

**Target throughput:** 5,000 TPS (transactions per second)

---

## 2. Architecture Diagram

```
Client (POS / Payment Gateway SDK)
        │
        ▼  gRPC : SubmitTransaction
┌──────────────────────┐
│     API Gateway      │  ← Validate request
│  (port 50051 gRPC)   │    Hash PAN → card_hash (HMAC-SHA256)
│                      │    Generate txn_id (UUID v4)
│                      │    Idempotency check (Redis DB1)
│                      │    Publish → Kafka topic: transactions
│                      │    Return TransactionAck immediately (PENDING)
└──────────────────────┘
        │
        ▼  Kafka topic: transactions  (partition key: card_hash)
        │  Partitions: 12 | Retention: 3 days | acks=all
        │
┌──────────────────────┐
│    Fraud Engine      │  ← Consumer group: fraud-engine-cg
│  Step 3.1: Rules     │    Fast synchronous checks (8 rules)
│  Step 3.2: Velocity  │    Redis sorted sets per card (7 velocity checks)
│  Step 3.3: Features  │    Build 11-field feature vector
│  Step 3.4: gRPC ──► │
└──────────────────────┘
        │
        ▼  gRPC : CalculateRisk  (P99 < 50 ms)
┌──────────────────────┐
│    Risk Service      │  ← Weighted linear scoring model
│  (port 50052 gRPC)   │    Returns: risk_score (100–1000) + decision
└──────────────────────┘
        │
        ▼  RiskResponse (risk_score, decision, triggered_rules)
┌──────────────────────┐
│    Fraud Engine      │  ← Merge triggered_rules + risk_score
│  (continued)         │    Final Decision: APPROVE / REVIEW / DECLINE
│                      │    Publish FraudResultEvent
└──────────────────────┘
        │
        ▼  Kafka topic: fraud_results  (partition key: card_hash)
        │  Partitions: 12 | Retention: 7 days | acks=all
        │
┌──────────────────────┐
│  Persistence Service │  ← Consumer group: persistence-svc-cg
│                      │    Upsert into PostgreSQL (transactions table)
│                      │    On DB failure: retry 3× → transaction_dlq
└──────────────────────┘
        │
        ▼
   PostgreSQL DB         ← Partitioned by txn_timestamp (monthly)
                            Indexed: card_hash, rrn, status, card+time
        │
        ▼
Prometheus → Grafana     ← Each service exposes /metrics (Prometheus format)
```

---

## 3. End-to-End Transaction Flow

### Step 0 — Client Request

Client sends a `SubmitTransaction` gRPC call to the API Gateway:

```json
{
  "pan": "4111111111111111",
  "expiry_month": 12,
  "expiry_year": 2027,
  "amount_minor": 500000,
  "currency_code": "OMR",
  "merchant_id": "M00000000000123",
  "terminal_id": "T0000456",
  "transaction_type": "SALES",
  "channel": "POS",
  "merchant_category_code": "5411",
  "retrieval_reference_number": "123456789012",
  "idempotency_key": "uuid-v4-from-client"
}
```

### Step 1 — API Gateway

| Action | Detail |
|---|---|
| Rate limit check | Per-client token bucket (100 rps, burst 200) |
| Validate fields | PAN (Luhn), amount, currency, channel, MCC, expiry, RRN format |
| Hash PAN | `card_hash = HMAC-SHA256(SHA-256(pan), PAN_HASH_SECRET)` → 64-char hex |
| Masked PAN | `411111XXXXXX1111` (first 6 + last 4, middle replaced with X) |
| Generate `txn_id` | UUID v4 |
| Idempotency | Redis DB1 `SET NX EX 86400` on key `idempotency:{key}` |
| Duplicate detection | If key exists → return original txn_id + `ALREADY_EXISTS` |
| Publish to Kafka | Topic: `transactions`, key: `card_hash` (for partition locality) |
| Return ACK | `{ txn_id, status: PENDING }` — non-blocking, P99 < 50 ms |

**Raw PAN never leaves the API Gateway.** It is not written to Kafka, logs, or any store.

### Step 2 — Kafka (transactions topic)

| Setting | Value |
|---|---|
| Partitions | 12 |
| Replication | 3 (survives 1 broker loss) |
| min.insync.replicas | 2 |
| Partition key | `card_hash` — per-card ordering guaranteed |
| Retention | 3 days |
| Compression | lz4 |
| Producer | `acks=all`, `enable.idempotence=true` |

### Step 3 — Fraud Engine

**Step 3.1 — Rule Engine (synchronous, in-memory)**

| Rule ID | What It Checks | Score Contribution |
|---|---|---|
| HIGH_AMOUNT | Amount > per-currency threshold (e.g. >500 OMR, >50K INR, >5K USD) | 200 |
| ROUND_AMOUNT | Suspiciously round large amount (structuring attack signal) | 80 |
| RISKY_MCC | Gambling, ATM cash, wire transfer, escort, crypto MCCs | 150 |
| CRYPTO_EXCHANGE_MCC | Cryptocurrency exchange MCCs (6051, 6099) | 120 |
| NIGHT_TRANSACTION | 00:00–05:00 UTC (Gulf midnight–dawn) | 60 |
| WEEKEND_HIGH_AMOUNT | Weekend + HIGH_AMOUNT both triggered | 40 |
| CARD_NOT_PRESENT | Online / PG channel transaction | 50 |
| FOREIGN_CURRENCY | Currency not in domestic list (OMR/AED/SAR/BHD/KWD/QAR) | 70 |

**Step 3.2 — Velocity Engine (Redis sorted sets)**

| Rule ID | Window | Threshold | Score |
|---|---|---|---|
| TXN_COUNT_1MIN | 60 s | 5 txns | 250 |
| TXN_COUNT_5MIN | 5 min | 10 txns | 150 |
| TXN_COUNT_1HR | 60 min | 20 txns | 80 |
| AMOUNT_SUM_1MIN | 60 s | 100,000 minor (100 OMR) | 200 |
| AMOUNT_SUM_1HR | 60 min | 1,000,000 minor (1000 OMR) | 100 |
| UNIQUE_MERCHANTS_5MIN | 5 min | 3 distinct merchants (HyperLogLog) | 180 |
| UNIQUE_TERMINALS_5MIN | 5 min | 4 distinct terminals (HyperLogLog) | 120 |

Redis key patterns:
- Count rules: `velocity:count:{rule_id}:{card_hash}` (sorted set, score=epoch-ms)
- Amount rules: `velocity:amount:{rule_id}:{card_hash}` (sorted set + companion HASH)
- HyperLogLog: `hll:merchants:{card_hash}:{window_bucket}`, `hll:terminals:{card_hash}:{window_bucket}`

**Step 3.3 — Feature Vector Preparation**

The Fraud Engine assembles an 11-field feature vector for the Risk Service:

```json
{
  "txn_count_1min":       5,
  "txn_count_5min":       8,
  "txn_count_1hr":        12,
  "amount_sum_1min":      500000,
  "amount_sum_1hr":       9500000,
  "unique_merchants_5min": 3,
  "unique_terminals_5min": 2,
  "merchant_risk":        0.8,
  "is_night":             0,
  "is_risky_mcc":         1,
  "is_card_not_present":  0
}
```

Merchant risk scores (0.0–1.0) are pre-loaded from PostgreSQL into Redis at startup
(`merchant_risk:{merchant_id}`, TTL 24 h). Default for unknown merchants: **0.5**.

**Step 3.4 — gRPC Call to Risk Service**

`RiskRequest` carries `txn_id`, `card_hash`, `amount_minor`, `currency`, `merchant_id`, `mcc`, `transaction_type`, `channel`, and the full `FraudFeatures` message.

gRPC call timeout: **500 ms**. Circuit breaker protects against Risk Service outages.

### Step 4 — Risk Service

Weighted linear scoring model:

```
risk_score = Σ ( normalised(feature_value) × weight ) × 1000
```

| Feature | Weight | Normalisation |
|---|---|---|
| amount_minor | 20% | log10(x) / 12 |
| txn_count_1min | 15% | linear cap at 20 |
| amount_sum_1min | 15% | log10(x) / log10(1B) |
| txn_count_5min | 10% | linear cap at 30 |
| unique_merchants_5min | 10% | linear cap at 10 |
| unique_terminals_5min | 7% | linear cap at 10 |
| amount_sum_1hr | 8% | log10(x) / log10(10B) |
| txn_count_1hr | 5% | linear cap at 50 |
| merchant_risk | 5% | passthrough (0.0–1.0) |
| is_night | 2% | binary |
| is_risky_mcc | 2% | binary |
| is_card_not_present | 1% | binary |

Returns:
```json
{
  "risk_score": 742,
  "decision": "DECLINE",
  "triggered_rules": ["HIGH_AMOUNT", "RISKY_MCC"]
}
```

### Step 5 — Fraud Engine: Final Decision

| Score Range | Decision |
|---|---|
| < 400 | APPROVE |
| 400 – 700 | REVIEW |
| > 700 | DECLINE |

Publishes `FraudResultEvent` to Kafka topic `fraud_results`.
The event is **fully enriched** — it carries the complete transaction payload plus the fraud decision. The Persistence Service does not need to join with the `transactions` topic.

### Step 6 — Persistence Service

Consumes from `fraud_results` (consumer group: `persistence-svc-cg`).

Upserts into PostgreSQL:
```sql
INSERT INTO transactions (txn_id, card_hash, masked_pan, amount_minor, currency,
    merchant_id, terminal_id, mcc, transaction_type, channel, scheme, rrn,
    txn_timestamp, status, risk_score, triggered_rules, processed_at)
VALUES (...)
ON CONFLICT (txn_id, txn_timestamp)
DO UPDATE SET
    status = EXCLUDED.status,
    risk_score = EXCLUDED.risk_score,
    triggered_rules = EXCLUDED.triggered_rules,
    processed_at = EXCLUDED.processed_at,
    updated_at = NOW();
```

On DB failure: exponential backoff retry (3 attempts, ~15 s total) → `transaction_dlq` (14-day retention).

### Step 7 — Metrics

Every service exposes `/metrics` → Prometheus → Grafana.

Key metrics:
- `sentinel_idempotency_redis_miss_total` — Redis unavailable events at gateway
- `sentinel_cb_state_transitions_total{from_state, to_state}` — circuit breaker transitions
- `sentinel_cb_fallback_decisions_total` — times fallback REVIEW was issued
- `sentinel_cb_open_duration_seconds` — gauge: how long circuit has been OPEN
- `sentinel_risk_score_histogram{decision}` — risk score distribution
- `sentinel_persistence_upserts_total{status}` — DB write success/retry/DLQ counts
- Kafka consumer lag per consumer group

---

## 4. Failure Scenarios

### Case 1 — Fraud Engine Crash

Kafka retains the message (3-day retention). Another consumer instance in `fraud-engine-cg` picks it up. Manual commit (`enable.auto.commit=false`) means no offset is acknowledged until processing completes. **No data loss.**

### Case 2 — Risk Service Down (Circuit Breaker)

| Setting | Value |
|---|---|
| Failure threshold | Open after **5 consecutive** failures |
| gRPC call timeout | **500 ms** per call |
| Fallback risk score | **600** → Decision: **REVIEW** |
| Open duration | Stay OPEN **15 s**, then probe with HALF_OPEN |
| Close threshold | **2 consecutive successes** in HALF_OPEN → CLOSE |
| State storage | Redis DB2 keys: `cb:risk_service:state`, `cb:risk_service:failures` |
| State TTL | 60 s (OPEN auto-expires → HALF_OPEN) |
| Shared state | All Fraud Engine replicas share circuit state via Redis |

**Design principle:** Never auto-DECLINE on infrastructure failure. Fallback is always REVIEW.

**Observability:**
- Structured JSON log on every state transition (`circuit_state`, `consecutive_failures`, `fallback_used`, `risk_service_error`)
- Prometheus alert fires if circuit stays OPEN > 30 s (severity: critical)

### Case 3 — PostgreSQL Down (DLQ)

Persistence Service retry policy:
- Attempt 1: immediate
- Attempt 2: 5 s after attempt 1 failure
- Attempt 3: 10 s after attempt 2 failure (~15 s total)
- After 3 failures: publish to `transaction_dlq`

DLQ message headers:

| Header | Value |
|---|---|
| `dlq-original-topic` | `fraud_results` |
| `dlq-original-offset` | `<offset>` |
| `dlq-original-partition` | `<partition>` |
| `dlq-failure-reason` | `<exception class>` |
| `dlq-retry-count` | `3` |
| `dlq-failed-at` | ISO 8601 UTC |

DLQ retention: **14 days**. Consumer group: `dlq-retry-worker-cg`.

### Case 4 — Duplicate Request

API Gateway checks Redis idempotency key before publishing to Kafka.
Duplicate within 24 h → returns original `TransactionAck` with gRPC status `ALREADY_EXISTS`.
**No duplicate event is ever published to Kafka.**

---

## 5. All Files Built

### Infrastructure / Config

| File | Purpose |
|---|---|
| [config/kafka-topics.yaml](config/kafka-topics.yaml) | 3 Kafka topics (transactions, fraud_results, transaction_dlq), partitions, retention, 4 consumer group configs, producer defaults |
| [config/redis.yaml](config/redis.yaml) | Redis connection (cluster + standalone), all 4 use cases (idempotency, velocity, merchant risk, circuit breaker), 3 DB assignments, eviction policies, memory sizing estimates |
| [config/fraud-rules.yaml](config/fraud-rules.yaml) | 8 rule definitions, 7 velocity checks, decision thresholds, 11-field feature vector spec, merchant risk defaults, circuit breaker config with Prometheus alert hints |
| [config/api-gateway.yaml](config/api-gateway.yaml) | gRPC server, PAN hashing, idempotency, Kafka producer, validation rules, rate limiting, logging |
| [config/fraud-engine.yaml](config/fraud-engine.yaml) | Kafka consumer + producer, Redis DB assignments, rule hot-reload, merchant risk warm-up, Risk Service gRPC client + circuit breaker, worker pool concurrency |
| [config/persistence-svc.yaml](config/persistence-svc.yaml) | Kafka consumer, DLQ producer, PostgreSQL connection pool + upsert config, batch upsert, retry policy, health/metrics |
| [config/risk-service.yaml](config/risk-service.yaml) | gRPC server, decision thresholds, weighted scoring model (12 features, weights, normalisation), contributing factor threshold |
| [config/schema-registry.yaml](config/schema-registry.yaml) | Schema Registry URL, 2 subjects (transactions-value, fraud_results-value), compatibility mode, CLI registration commands |

### Protobuf Contracts

| File | Package | Purpose |
|---|---|---|
| [proto/gateway.proto](proto/gateway.proto) | `sentinel.gateway.v1` | Client → API Gateway: `SubmitTransaction`, `GetTransactionStatus` RPCs |
| [proto/transactions.proto](proto/transactions.proto) | `sentinel.transactions.v1` | `TransactionEvent` — Kafka message API Gateway → Fraud Engine |
| [proto/risk.proto](proto/risk.proto) | `sentinel.risk.v1` | `RiskService.CalculateRisk` — Fraud Engine → Risk Service gRPC |
| [proto/fraud_results.proto](proto/fraud_results.proto) | `sentinel.fraudresults.v1` | `FraudResultEvent` — Kafka message Fraud Engine → Persistence Service |

### Database

| File | Purpose |
|---|---|
| [db/migrations/001_create_transactions.sql](db/migrations/001_create_transactions.sql) | `transactions` table (PARTITION BY RANGE on txn_timestamp), 2025+2026 monthly partitions, 5 indexes, `updated_at` trigger, upsert template |

### API Gateway Service Code (Go)

| File | Purpose |
|---|---|
| [services/api-gateway/cmd/main.go](services/api-gateway/cmd/main.go) | Entry point: config load, dependency wiring, gRPC server + health + metrics HTTP server, graceful shutdown |
| [services/api-gateway/internal/config/config.go](services/api-gateway/internal/config/config.go) | Typed config structs for all sections; `${VAR:-default}` env expansion; loads `api-gateway.yaml` + `redis.yaml` |
| [services/api-gateway/internal/gateway/handler.go](services/api-gateway/internal/gateway/handler.go) | gRPC handler: rate limit → validate → hash PAN → idempotency → publish Kafka → return ACK |
| [services/api-gateway/internal/gateway/validator.go](services/api-gateway/internal/gateway/validator.go) | Full field validation: PAN (13–19 digits, Luhn check), expiry, amount range, currency/type/channel allow-lists, MCC/RRN regex |
| [services/api-gateway/internal/hashing/pan.go](services/api-gateway/internal/hashing/pan.go) | `card_hash = HMAC-SHA256(SHA-256(pan), secret)` → 64-char hex; `MaskedPAN(pan)` → `411111XXXXXX1111`; `Verify()` with timing-safe compare |
| [services/api-gateway/internal/idempotency/redis.go](services/api-gateway/internal/idempotency/redis.go) | Redis-backed idempotency: `SET NX EX` (atomic); returns original txn_id on duplicate; configurable allow/reject on Redis failure |
| [services/api-gateway/internal/kafka/producer.go](services/api-gateway/internal/kafka/producer.go) | Confluent wire format (magic byte + schema_id + proto payload); lazy schema ID fetch from Schema Registry; `kafka-go` writer |
| [services/api-gateway/internal/ratelimit/limiter.go](services/api-gateway/internal/ratelimit/limiter.go) | Per-client token bucket (`golang.org/x/time/rate`); client identity from gRPC metadata header or peer address; idle entry eviction |
| [services/api-gateway/Dockerfile](services/api-gateway/Dockerfile) | Multi-stage: `golang:1.23-alpine` builder → `scratch` runtime; static binary (CGO_ENABLED=0); non-root UID 65532; gRPC:9090 metrics:9091 |
| [services/api-gateway/go.mod](services/api-gateway/go.mod) | Module: `github.com/sentinelswitch/api-gateway`; Go 1.22; key deps: `kafka-go`, `go-redis/v9`, `grpc`, `uuid`, `time/rate`, `yaml.v3` |

### Fraud Engine Service Code (Go)

| File | Purpose |
|---|---|
| [services/fraud-engine/cmd/main.go](services/fraud-engine/cmd/main.go) | Entry point: wires Redis clients (3 DBs), rules engine, velocity store, circuit breaker, risk gRPC client, pipeline processor; metrics + health HTTP servers; graceful shutdown |
| [services/fraud-engine/internal/config/config.go](services/fraud-engine/internal/config/config.go) | Typed config structs for Kafka consumer/producer, Redis multi-DB, rules hot-reload, Risk Service gRPC, circuit breaker, server ports |
| [services/fraud-engine/internal/rules/engine.go](services/fraud-engine/internal/rules/engine.go) | Rule engine: evaluates 8 synchronous rules (HIGH_AMOUNT, ROUND_AMOUNT, RISKY_MCC, CRYPTO_EXCHANGE_MCC, NIGHT_TRANSACTION, WEEKEND_HIGH_AMOUNT, CARD_NOT_PRESENT, FOREIGN_CURRENCY); returns triggered rule IDs + scores; hot-reload from YAML |
| [services/fraud-engine/internal/velocity/store.go](services/fraud-engine/internal/velocity/store.go) | Velocity engine: 7 Redis-backed checks using sorted sets (ZRANGEBYSCORE window) + HyperLogLog for unique merchant/terminal counts; returns triggered rule IDs + scores |
| [services/fraud-engine/internal/features/builder.go](services/fraud-engine/internal/features/builder.go) | Assembles the 11-field `FraudFeatures` proto message from velocity counts, merchant risk score (Redis lookup with 0.5 default), and rule flags |
| [services/fraud-engine/internal/risk/client.go](services/fraud-engine/internal/risk/client.go) | gRPC client to Risk Service: connection pool (10 conns), 500 ms call timeout, builds `RiskRequest` from `TransactionEvent` + `FraudFeatures` |
| [services/fraud-engine/internal/circuitbreaker/breaker.go](services/fraud-engine/internal/circuitbreaker/breaker.go) | Redis-backed shared circuit breaker: CLOSED → OPEN (5 consecutive failures) → HALF_OPEN (15 s) → CLOSED (2 successes); fallback score 600/REVIEW; all replicas share state via Redis DB2 |
| [services/fraud-engine/internal/pipeline/processor.go](services/fraud-engine/internal/pipeline/processor.go) | Kafka consumer loop: deserialise `TransactionEvent` → rules → velocity → features → circuit-break → gRPC `CalculateRisk` → merge decisions → publish `FraudResultEvent` → manual commit |
| [services/fraud-engine/internal/redis/client.go](services/fraud-engine/internal/redis/client.go) | Redis client factory: creates `go-redis/v9` clients for DB0 (velocity + merchant risk) and DB2 (circuit breaker) from config |
| [services/fraud-engine/Dockerfile](services/fraud-engine/Dockerfile) | Multi-stage: `golang:1.23-alpine` builder → `scratch` runtime; static binary; non-root UID 65532 |
| [services/fraud-engine/go.mod](services/fraud-engine/go.mod) | Module: `github.com/sentinelswitch/fraud-engine`; Go 1.22; key deps: `kafka-go`, `go-redis/v9`, `grpc`, `prometheus/client_golang`, `zap` |

### Risk Service Code (Go)

| File | Purpose |
|---|---|
| [services/risk-service/cmd/main.go](services/risk-service/cmd/main.go) | Entry point: loads config, wires scorer + gRPC handler, starts gRPC server (port 50052) + metrics HTTP server; graceful shutdown on SIGTERM/SIGINT |
| [services/risk-service/internal/config/config.go](services/risk-service/internal/config/config.go) | Typed config structs for gRPC server, decision thresholds, 12-feature weighted scoring model (weights, normalisation function names, caps) |
| [services/risk-service/internal/scoring/scorer.go](services/risk-service/internal/scoring/scorer.go) | Weighted linear scorer: normalises each feature (log10 / linear cap / binary / passthrough), multiplies by weight, sums to 0–1, scales to 100–1000; extracts contributing factors above 5% threshold |
| [services/risk-service/internal/server/handler.go](services/risk-service/internal/server/handler.go) | gRPC `RiskService` server: validates `RiskRequest`, invokes scorer, maps score to APPROVE/REVIEW/DECLINE decision, returns `RiskResponse` with `triggered_rules` |
| [services/risk-service/Dockerfile](services/risk-service/Dockerfile) | Multi-stage: `golang:1.23-alpine` → `scratch`; static binary; non-root UID 65532; exposes gRPC:50052 + metrics |
| [services/risk-service/go.mod](services/risk-service/go.mod) | Module: `github.com/sentinelswitch/risk-service`; Go 1.22; key deps: `grpc`, `protobuf`, `prometheus/client_golang`, `zap` |

### Persistence Service Code (Go)

| File | Purpose |
|---|---|
| [services/persistence-svc/cmd/main.go](services/persistence-svc/cmd/main.go) | Entry point: loads config, opens PostgreSQL pool, wires DLQ producer + pipeline processor; metrics + health HTTP servers; graceful shutdown |
| [services/persistence-svc/internal/config/config.go](services/persistence-svc/internal/config/config.go) | Typed config structs for Kafka consumer, DLQ producer, PostgreSQL connection pool + upsert settings, retry policy, server ports |
| [services/persistence-svc/internal/kafka/dlq.go](services/persistence-svc/internal/kafka/dlq.go) | DLQ Kafka producer: publishes failed events to `transaction_dlq` with enriched headers (`dlq-original-topic`, `-offset`, `-partition`, `-failure-reason`, `-retry-count`, `-failed-at`) |
| [services/persistence-svc/internal/pipeline/processor.go](services/persistence-svc/internal/pipeline/processor.go) | Kafka consumer loop: deserialise `FraudResultEvent` → batch accumulation → flush on size/time threshold → upsert to PostgreSQL → retry (3×, exponential backoff) → DLQ on terminal failure; manual offset commit |
| [services/persistence-svc/internal/store/postgres.go](services/persistence-svc/internal/store/postgres.go) | PostgreSQL upsert: `INSERT … ON CONFLICT (txn_id, txn_timestamp) DO UPDATE`; batch upsert support; distinguishes retryable (transient) vs non-retryable (constraint/invalid) errors |
| [services/persistence-svc/Dockerfile](services/persistence-svc/Dockerfile) | Multi-stage: `golang:1.23-alpine` → `scratch`; static binary; non-root UID 65532 |
| [services/persistence-svc/go.mod](services/persistence-svc/go.mod) | Module: `github.com/sentinelswitch/persistence-svc`; Go 1.22; key deps: `kafka-go`, `pgx/v5`, `prometheus/client_golang`, `zap` |

### Proto Code Generation

| File | Purpose |
|---|---|
| [buf.yaml](buf.yaml) | Buf workspace root — points to `proto/` directory |
| [buf.gen.yaml](buf.gen.yaml) | Generates Go + gRPC code into `services/proto-gen/` via `protoc-gen-go` + `protoc-gen-go-grpc` |
| [services/proto-gen/go.mod](services/proto-gen/go.mod) | Module: `github.com/sentinelswitch/proto`; Go 1.22; depends on `grpc` + `protobuf` |

---

## 6. Protobuf Contracts (proto/)

### gateway.proto — GatewayService

**RPCs:**

| RPC | Request | Response | SLA |
|---|---|---|---|
| `SubmitTransaction` | `TransactionRequest` | `TransactionAck` | P99 < 50 ms |
| `GetTransactionStatus` | `StatusRequest` | `TransactionStatusResponse` | P99 < 30 ms (Redis cache) |

**Key fields in `TransactionRequest`:**
- `pan_last4`, `card_expiry`, `card_hash` — card identity (raw PAN never sent in proto; hash computed by SDK/terminal)
- `amount_minor` (int64) + `currency` — avoids float rounding
- `merchant_id` — derived from JWT at gateway; clients cannot override
- `idempotency_key` — clients must supply UUID v4; 24-h deduplication window

**Error codes:** `INVALID_ARGUMENT`, `UNAUTHENTICATED`, `ALREADY_EXISTS`, `RESOURCE_EXHAUSTED`, `NOT_FOUND`, `INTERNAL`, `UNAVAILABLE`

### transactions.proto — TransactionEvent (Kafka)

Published by API Gateway, consumed by Fraud Engine.
Partition key: `card_hash` — per-card ordering.
Contains: identity, card (hash + masked), amount (minor units), merchant/terminal, classification (type/channel/scheme), RRN.

### risk.proto — RiskService

**RPC:** `CalculateRisk(RiskRequest) → RiskResponse`

`RiskRequest` contains the full `FraudFeatures` message (field 12) with 11 velocity/rule fields.

Fields 7–9 are **reserved** (`velocity_window`, `velocity_count`, `velocity_amount`) — superseded by `FraudFeatures`. Field numbers must not be reused.

`RiskResponse`:
- `risk_score`: int32 (100–1000)
- `decision`: enum (APPROVE / DECLINE / REVIEW)
- `triggered_rules`: repeated string

**Score bands:**
100–399 APPROVE | 400–700 REVIEW | 701–1000 DECLINE

### fraud_results.proto — FraudResultEvent (Kafka)

Published by Fraud Engine, consumed by Persistence Service.
**Fully enriched event** — carries the complete transaction payload (mirrored from TransactionEvent) plus fraud decision fields (`decision`, `risk_score`, `triggered_rules`, `processed_at`).
Design rationale: eliminates consumer join; prevents race condition where result arrives before raw event is written.

---

## 7. Configuration Files (config/)

### kafka-topics.yaml

| Topic | Partitions | Retention | Producer | Consumers |
|---|---|---|---|---|
| `transactions` | 12 | 3 days | API Gateway | Fraud Engine (`fraud-engine-cg`) |
| `fraud_results` | 12 | 7 days | Fraud Engine | Persistence Svc (`persistence-svc-cg`), Ops Dashboard (`ops-dashboard-cg`) |
| `transaction_dlq` | 3 | 14 days | Persistence Svc | DLQ retry worker (`dlq-retry-worker-cg`) |

All topics: replication factor 3, min.insync.replicas 2, compression lz4.

### redis.yaml

| DB | Use Case | Key Pattern | TTL | Eviction Policy |
|---|---|---|---|---|
| DB0 | Velocity counters | `velocity:count:{rule_id}:{card_hash}` | window + 60 s | `allkeys-lru` |
| DB0 | Merchant risk | `merchant_risk:{merchant_id}` | 86400 s | `allkeys-lru` |
| DB1 | Idempotency | `idempotency:{txn_id}` | 86400 s | **`noeviction`** (critical) |
| DB2 | Circuit breaker state | `cb:risk_service:state` | 60 s | `volatile-lru` |
| DB2 | Circuit breaker failures | `cb:risk_service:failures` | 30 s | `volatile-lru` |

**Memory sizing at 5K TPS:** Idempotency alone requires ~34 GB at full 24-h retention; Redis Cluster is mandatory. Velocity sorted sets: 5–10 GB for active cards.

### fraud-rules.yaml

- 8 synchronous rules with score contributions, conditions (amount thresholds per currency, MCC lists, time windows, channel matches)
- 7 velocity rules with window seconds, count/amount thresholds, Redis key patterns
- Decision thresholds: approve_below=400, decline_above=700
- Circuit breaker: failure_threshold=5, open_duration=15 s, fallback=600/REVIEW
- Feature vector: 11 fields mapping to velocity, merchant risk, rule flag sources
- Hot-reload: file-watcher checks every 30 s, no restart needed

### api-gateway.yaml

- gRPC: port 50051, TLS configurable, keepalive tuned, 1 MB max message size
- PAN hashing: HMAC-SHA256, `PAN_HASH_SECRET` env var, prefix 6 + suffix 4 masked
- Idempotency: Redis DB1, `SET NX EX 86400`, allow-through on Redis failure
- Kafka producer: topic `transactions`, `acks=all`, lz4, linger 5 ms, 64 KB batch
- Validation: 8 currencies, 4 transaction types, 4 channels, MCC `^[0-9]{4}$`, RRN `^[0-9]{12}$`
- Rate limiting: 100 rps / burst 200 per client (token bucket)

### fraud-engine.yaml

- Kafka consumer: group `fraud-engine-cg`, topic `transactions`, manual commit, max 500 records
- Kafka producer: topic `fraud_results`, `acks=all`, `enable_idempotence=true`
- Merchant risk warm-up: loads up to 100K rows from PostgreSQL → Redis at startup
- Risk Service gRPC: port 50052, call timeout 500 ms, pool 10 connections
- Circuit breaker: Redis DB2 shared state across all FE replicas
- Worker pool: 100 goroutines, max 50 concurrent Risk Service calls

### persistence-svc.yaml

- Kafka consumer: group `persistence-svc-cg`, topic `fraud_results`, manual commit, max 1000 records
- DLQ producer: topic `transaction_dlq`, `acks=all`, `enable_idempotence=true`
- PostgreSQL: connection pool (20 open, 5 idle), batch upsert (500 rows, 500 ms flush)
- Retry: 3 attempts, 5 s → 10 s backoff (~15 s total), then DLQ
- Non-retryable: `constraint_violation`, `invalid_input` → immediate DLQ

### risk-service.yaml

- gRPC: port 50052, max 500 concurrent streams
- Scoring: weighted linear model, 12 features, weights sum to 1.00
- Decision thresholds mirror fraud-rules.yaml (service validates consistency at startup)
- Contributing factor threshold: 5% of max score for explainability output

---

## 8. Database Layer (db/)

### 001_create_transactions.sql

**Table design:**
- `transactions` partitioned by `txn_timestamp` (RANGE, monthly)
- Primary key: `(txn_id, txn_timestamp)` — partition key must be in PK
- Amount stored as `BIGINT amount_minor` — no float rounding (OMR 5.250 → 5250)
- No raw PAN — only `card_hash VARCHAR(64)` + `masked_pan VARCHAR(19)`
- `triggered_rules JSONB` — stores array like `["HIGH_AMOUNT", "VELOCITY_SPIKE"]`

**Monthly partitions pre-created:** 2025-01 through 2026-12 + `transactions_default` catch-all

**Indexes:**
- `idx_txn_card_hash` — velocity lookups by card
- `idx_txn_timestamp` — date-range queries + partition pruning
- `idx_txn_rrn` — reconciliation lookups
- `idx_txn_status` — ops monitoring queries
- `idx_txn_card_time (card_hash, txn_timestamp DESC)` — per-card time-series (velocity, reporting)

**Trigger:** `trg_transactions_updated_at` (BEFORE UPDATE) keeps `updated_at` current.
Note: Requires PostgreSQL 14+ for row triggers on partitioned parent tables.

---

## 9. API Gateway Service (services/api-gateway/)

The API Gateway is the only fully implemented service. It is written in **Go 1.22**.

### Dependency Graph

```
main.go
 ├── config.Load()          → reads api-gateway.yaml + redis.yaml
 ├── hashing.New()          → PAN hasher
 ├── idempotency.New()      → Redis-backed idempotency store
 ├── kafka.New()            → Confluent-format Kafka producer
 ├── ratelimit.New()        → per-client token bucket
 ├── gateway.NewValidator() → field validator
 └── gateway.NewHandler()   → gRPC handler (wires all above)
```

### Package Breakdown

**`internal/config/config.go`**
- Typed structs matching every section in `api-gateway.yaml` and `redis.yaml`
- `${VAR:-default}` env expansion via custom regex (no external templating)
- Validates mandatory env var `PAN_HASH_SECRET` at startup — service refuses to start if unset
- Strips YAML inline comments before parsing (avoids parser issues with `#` in values)

**`internal/hashing/pan.go`**
- `Hash(pan) string` — `HMAC-SHA256(SHA-256(pan), secret)` → 64-char lowercase hex
- `Verify(pan, candidateHash)` — timing-safe comparison via `hmac.Equal`
- `MaskedPAN(pan) string` — configurable prefix/suffix lengths + mask character
- `MaskedPANFromLast4(last4)` — builds masked PAN when only last 4 digits available

**`internal/idempotency/redis.go`**
- `CheckAndSet(ctx, idempotencyKey, txnID)` — `SET NX EX` then `GET` on miss
- Race condition handled: if key expires between `SetNX` and `GET` → treats as new request
- Two modes: `allow` (degrade gracefully on Redis failure) or `reject` (return error)
- `Ping(ctx)` for startup liveness check

**`internal/kafka/producer.go`**
- **Confluent wire format**: `0x00` magic byte + 4-byte big-endian schema_id + proto bytes
- Schema ID lazy-fetched from Schema Registry on first `Publish` call, then cached
- HTTP client with 3 s timeout for schema registry calls
- Wraps `segmentio/kafka-go` Writer with full producer config support

**`internal/ratelimit/limiter.go`**
- Per-client `rate.Limiter` (token bucket from `golang.org/x/time/rate`)
- Client key from configurable gRPC metadata header → fallback to peer address
- Background goroutine evicts entries idle > 10 min every 5 min (prevents unbounded growth)

**`internal/gateway/validator.go`**
- Collects all validation errors before returning (returns all failures, not first only)
- PAN: strip spaces/dashes, length 13–19, digits-only, Luhn algorithm
- Expiry: month 1–12, year 2024–2099
- Amount: configurable min/max minor units
- Currency/type/channel: case-insensitive allow-list from config
- MCC + RRN: compiled regex patterns from config

**`internal/gateway/handler.go`**
- `SubmitTransaction`: rate limit → validate → hash PAN → masked PAN → idempotency check → build `TransactionEvent` proto → publish to Kafka → return ACK
- `GetTransactionStatus`: stub (status queries delegate to transaction-processor service)
- PAN never stored after `Hash()` call — not in the Kafka event, not in logs

**`cmd/main.go`**
- gRPC server with middleware chain: zap request logging + panic recovery
- gRPC health protocol (`grpc_health_v1`) for Kubernetes probes
- gRPC reflection registered when enabled (dev/grpcurl support)
- HTTP server on separate port: `/metrics` (Prometheus) + `/healthz` (liveness)
- Graceful shutdown: SIGINT/SIGTERM → health → `GracefulStop()` → HTTP shutdown → close producer + idempotency store (30 s timeout)

**`Dockerfile`**
- Builder: `golang:1.23-alpine` (git, ca-certs, tzdata)
- Static binary: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, `-s -w` ldflags, version/commit embedded
- Runtime: `FROM scratch` (minimal attack surface)
- Non-root: UID/GID 65532
- Exposes 9090 (gRPC), 9091 (metrics/health)

---

## 10. Fraud Engine Service (services/fraud-engine/)

### Service Responsibility
Consumes raw `TransactionEvent` messages from Kafka, applies synchronous rule checks, computes velocity metrics from Redis, assembles feature vectors, calls the Risk Service via gRPC, merges decisions, and publishes `FraudResultEvent` to Kafka.

### Startup Sequence

```
main.go
 ├── LoadConfig()            → fraud-engine.yaml
 ├── redis.NewVelocityClient() → DB0 (velocity + merchant risk scores)
 ├── redis.NewCBClient()       → DB2 (circuit breaker shared state)
 ├── rules.NewEngine()         → loads rules.yaml; hot-reload goroutine
 ├── velocity.NewStore()       → 7 Redis-backed velocity checks
 ├── features.NewBuilder()     → assembles FraudFeatures proto
 ├── circuitbreaker.New()      → CLOSED/OPEN/HALF_OPEN Redis state machine
 ├── risk.NewClient()          → gRPC connection pool to Risk Service
 └── pipeline.NewProcessor()   → Kafka consumer loop
```

### Package Breakdown

**`internal/rules/engine.go`**
- Evaluates 8 synchronous rules in order; each rule returns `(triggered bool, score float64)`
- Rules: `HIGH_AMOUNT`, `ROUND_AMOUNT`, `RISKY_MCC`, `CRYPTO_EXCHANGE_MCC`, `NIGHT_TRANSACTION`, `WEEKEND_HIGH_AMOUNT`, `CARD_NOT_PRESENT`, `FOREIGN_CURRENCY`
- Rule thresholds loaded from `rules.yaml`; hot-reload polls file every 60 s (configurable)
- `GetApplicableRules(txn)` returns slice of triggered rule IDs + their scores

**`internal/velocity/store.go`**
- 7 Redis-backed velocity checks using sorted sets (`ZADD`, `ZRANGEBYSCORE` time window)
- HyperLogLog (`PFADD`/`PFCOUNT`) for unique merchant count and unique terminal count estimates
- Checks: `VELOCITY_5M`, `VELOCITY_1H`, `DAILY_AMOUNT_LIMIT`, `CROSS_BORDER_VELOCITY`, `MERCHANT_VELOCITY`, `TERMINAL_VELOCITY`, `UNIQUE_MERCHANTS`
- Each check: key per `card_hash`, TTL set on first write, non-blocking (errors logged, check skipped)

**`internal/features/builder.go`**
- Assembles 11-field `FraudFeatures` proto from velocity counts + merchant risk score (Redis HGET, default 0.5) + rule trigger flags
- Fields: `txn_count_5m`, `txn_count_1h`, `total_amount_24h`, `unique_merchants_7d`, `unique_terminals_7d`, `avg_txn_amount`, `max_txn_amount`, `cross_border_count`, `merchant_risk_score`, `is_high_risk_mcc`, `is_card_not_present`

**`internal/risk/client.go`**
- gRPC `RiskService` client with `grpc.WithBlock()` connection + `grpc.PoolSize(10)` per config
- 500 ms call timeout via `context.WithTimeout`
- Builds `RiskRequest{transaction_event, fraud_features}` proto

**`internal/circuitbreaker/breaker.go`**
- State stored in Redis DB2 (key: `cb:fraud_engine:risk_service`)
- CLOSED → OPEN: 5 consecutive failures; OPEN → HALF_OPEN: 15 s cooldown; HALF_OPEN → CLOSED: 2 consecutive successes
- Fallback on OPEN: score = 600, decision = `REVIEW`, `triggered_rules = ["CIRCUIT_OPEN"]`
- All replicas share the same Redis state → no split-brain between Fraud Engine pods

**`internal/pipeline/processor.go`**
- Kafka consumer (group `fraud-engine-group`): reads `transaction_events` topic
- Per-message: `rules.GetApplicableRules` → `velocity.Check` → `features.Build` → circuit-break → `risk.CalculateRisk` → merge decisions → publish `FraudResultEvent`
- Manual offset commit (after successful publish) → at-least-once delivery
- Kafka writer for `fraud_results` topic with Confluent wire format + schema ID

**`internal/redis/client.go`**
- Factory for `go-redis/v9` clients using DB index from config

---

## 11. Risk Service (services/risk-service/)

### Service Responsibility
Stateless gRPC service. Receives `RiskRequest{TransactionEvent, FraudFeatures}`, applies a weighted linear scoring model, maps the score to a decision, and returns `RiskResponse{score, decision, triggered_rules}`.

### Startup Sequence

```
main.go
 ├── LoadConfig()          → risk-service.yaml
 ├── scoring.NewScorer()   → loads 12-feature weight/norm config
 └── server.NewHandler()   → gRPC RiskService implementation
```

### Package Breakdown

**`internal/scoring/scorer.go`**
- Weighted linear model with 12 features
- Normalisation functions per feature: `log10` (amount fields), `linear_cap` (velocity counts), `binary` (boolean flags), `passthrough` (already 0–1)
- Raw score = sum(normalised_value × weight), clamped to [0, 1], scaled to [100, 1000]
- Contributing factors: features whose weighted contribution > 5% of total score
- Thresholds (configurable): score ≥ 700 → DECLINE; score ≥ 450 → REVIEW; else APPROVE

**`internal/server/handler.go`**
- Validates `RiskRequest` (non-nil transaction + features)
- Calls `scorer.Score(features)`
- Maps score to `RiskDecision` enum; assembles `triggered_rules` from contributing factors
- Returns structured `RiskResponse` with `Prometheus` counter increments

---

## 12. Persistence Service (services/persistence-svc/)

### Service Responsibility
Consumes `FraudResultEvent` messages from Kafka, batches records, and upserts them into the `fraud_results` PostgreSQL table. Failed records are sent to a Dead Letter Queue (DLQ) Kafka topic after exhausting retries.

### Startup Sequence

```
main.go
 ├── LoadConfig()          → persistence-svc.yaml
 ├── store.NewPostgres()   → pgxpool connection pool, ping check
 ├── kafka.NewDLQ()        → Kafka producer targeting transaction_dlq
 └── pipeline.NewProcessor() → Kafka consumer + batch processor
```

### Package Breakdown

**`internal/pipeline/processor.go`**
- Kafka consumer group `persistence-svc-group`; reads `fraud_results` topic
- Batch accumulation: flush when batch reaches size threshold (default 100) OR ticker fires (default 5 s)
- Upsert batch via `store.UpsertBatch()`; on retryable error: retry 3× with exponential backoff (500 ms base, 2× multiplier)
- On terminal failure (non-retryable DB error or retries exhausted): send each failed event to DLQ producer
- Manual offset commit after successful upsert or DLQ acknowledgement → no data loss, no duplicate rows

**`internal/store/postgres.go`**
- `pgxpool` connection pool (max 20 conns, configurable)
- `UpsertOne(ctx, event)` — `INSERT … ON CONFLICT (txn_id, txn_timestamp) DO UPDATE SET …`
- `UpsertBatch(ctx, events)` — iterates and calls `UpsertOne`; collects per-event errors
- Retryable errors: `pgx.ErrNoRows`, `connection refused`, `pq: could not connect`
- Non-retryable: constraint violations, invalid data type errors — sent immediately to DLQ

**`internal/kafka/dlq.go`**
- Kafka writer targeting `transaction_dlq` topic
- Enriches each failed message with headers: `dlq-original-topic`, `dlq-original-offset`, `dlq-original-partition`, `dlq-failure-reason`, `dlq-retry-count`, `dlq-failed-at` (RFC3339)
- Configurable required acks + compression codec

---

## 13. Proto Code Generation (buf)

```
buf.yaml          → workspace config, points to proto/
buf.gen.yaml      → generates Go + gRPC stubs into services/proto-gen/
                    using protoc-gen-go + protoc-gen-go-grpc
                    output module: github.com/sentinelswitch/proto
```

**To regenerate after proto changes:**
```bash
buf generate
```

Generated files land in `services/proto-gen/` and are consumed by all services via `replace` directive in `go.mod`:
```
replace github.com/sentinelswitch/proto => ../proto-gen
```

---

## 14. Key Design Decisions

| Decision | Rationale |
|---|---|
| `amount_minor int64` everywhere | OMR has 3 decimal places (5.250 OMR = 5250 minor). `double` would introduce rounding errors. `int64` is exact. |
| Raw PAN never crosses service boundary | PCI-DSS compliance. Hashed at gateway entry, only `card_hash` + `masked_pan` travel downstream. |
| `card_hash` as Kafka partition key | All transactions for the same card land on the same partition → per-card ordering for velocity counting. |
| `FraudResultEvent` is fully enriched | Persistence Service reads only `fraud_results` — no join required, no race condition between topics. |
| Enums redeclared per proto package | Schema registry independence. Each package is self-contained and can evolve independently. |
| `reserved` fields in risk.proto (7,8,9) | Old `velocity_window/count/amount` fields superseded by `FraudFeatures`. Reserving prevents accidental reuse of field numbers. |
| Redis DB1 idempotency: `noeviction` | Silent LRU eviction of an idempotency key would cause duplicate Kafka events (double charge). `noeviction` forces the gateway to treat OOM as a hard error (503). |
| Circuit breaker fallback = 600/REVIEW | Never auto-DECLINE on infrastructure failure. A DECLINE is a hard customer-facing rejection. REVIEW queues for human inspection. |
| `FraudFeatures` assembled before gRPC call | Risk Service must not re-query Redis. All velocity data is gathered by Fraud Engine before the gRPC call, eliminating latency from the Risk Service hot path. |
| PostgreSQL partitioned by `txn_timestamp` | Enables partition pruning for date-range queries and efficient archival of old data. Monthly partitions pre-created for 2025–2026. |
| Conflict key `(txn_id, txn_timestamp)` | PostgreSQL partitioned upsert requires the partition key (`txn_timestamp`) to be in the conflict target alongside the business key (`txn_id`). |

---

## 15. What Remains to Build

| Layer | Component | Notes |
|---|---|---|
| Infrastructure | Docker Compose | Full local stack: Kafka (KRaft or ZooKeeper), Schema Registry, Redis (3-node cluster), PostgreSQL, all 4 services |
| Observability | prometheus.yml | Scrape targets for all 4 services + Kafka + Redis + PostgreSQL exporters |
| Observability | Grafana dashboards | Fraud decision rates, rule trigger rates, risk score histogram, consumer lag, circuit breaker state |
| Database | Migrations 002+ | Any schema evolution (new indexes, additional columns) |
| Testing | Unit tests | Rule engine logic, PAN hashing, Luhn check, validator, idempotency store |
| Testing | Integration tests | Full Kafka → Fraud Engine → Risk Service → Persistence → DB flow |
| Testing | Failure scenario tests | Circuit breaker behaviour, DLQ flow, duplicate request handling |
| Security | JWT validation middleware | API Gateway: validate Bearer JWT, extract `merchant_id` from claims |
| Security | mTLS | Service-to-service gRPC (Fraud Engine → Risk Service) |
| Deployment | Kubernetes manifests | Deployments, Services, ConfigMaps, HPA, PodDisruptionBudgets |
| Deployment | CI/CD pipeline | buf generate → go build → go test → docker build → push → deploy |
