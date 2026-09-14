# SentinelSwitch — Project Overview
**Ticket:** CREDO-ALERT-001

---

## 1. What Is This System?

SentinelSwitch is a **real-time fraud detection platform** for payment transactions.

A client (POS terminal / payment page) submits a transaction. The system:
1. Accepts it immediately (non-blocking ACK)
2. Runs fraud analysis asynchronously via a Kafka-driven pipeline
3. Stores the final decision in PostgreSQL
4. Exposes metrics via Prometheus → Grafana

Target throughput: **5,000 TPS (transactions per second)**

---

## 2. Full System Architecture

```
Client (POS / PG)
        │
        ▼ gRPC (SubmitTransaction)
┌───────────────────┐
│    API Gateway    │  ← Validate, Idempotency (Redis), Generate txn_id
│                   │    Publish → Kafka, Return ACK immediately
└───────────────────┘
        │
        ▼ Kafka topic: transactions  (partition key: card_hash)
┌───────────────────┐
│   Fraud Engine    │  ← Core brain
│  Step 3.1: Rules  │    Fast checks: amount, MCC, time, channel
│  Step 3.2: Velocity│   Redis time-window counters per card
│  Step 3.3: Features│   Build feature vector
│  Step 3.4: gRPC → │
└───────────────────┘
        │
        ▼ gRPC (CalculateRisk)
┌───────────────────┐
│   Risk Service    │  ← Weighted score calculation → returns risk_score + decision
└───────────────────┘
        │
        ▼ returns RiskResponse
┌───────────────────┐
│   Fraud Engine    │  ← Merges triggered_rules + risk_score → final Decision
│   (continued)    │    Publishes FraudResultEvent
└───────────────────┘
        │
        ▼ Kafka topic: fraud_results  (partition key: txn_id)
┌───────────────────┐
│ Persistence Svc   │  ← Upsert into PostgreSQL (transactions table)
│                   │    On DB failure → transaction_dlq (after 3 retries)
└───────────────────┘
        │
        ▼
   PostgreSQL DB      ← Date-partitioned, indexed for velocity + reconciliation
        │
        ▼
Prometheus → Grafana  ← Each service exposes /metrics




Client
  ↓
API Gateway
  - Validate
  - Idempotency
  - Publish to Kafka
  ↓
Kafka (transactions topic)
  ↓
Fraud Engine
  - Rules
  - Velocity (Redis)
  - gRPC → Risk Service
  ↓
Risk Service
  - Score calculation
  ↓
Fraud Engine
  - Final decision
  - Publish fraud_results
  ↓
Kafka (fraud_results)
  ↓
Persistence Service
  - Store in PostgreSQL
  ↓
Metrics → Prometheus → Grafana

```

---

## 3. Step-by-Step Flow

### Step 0 — Client Sends Transaction

```json
{
  "txn_id": "uuid",
  "card_hash": "string",
  "amount": 5000,
  "currency": "INR",
  "merchant_id": "M00000000000123",
  "terminal_id": "T0000456",
  "scheme":"mastercard",
  "transaction_type":"sales",
  "channel":"pos",
  "rrn": "123456789012",
  "mcc": "5411",
  "txn_timestamp": "ISO8601"
}
```

---

### Step 1 — API Gateway

| Action | Detail |
|---|---|
| Validate fields | amount, card, currency |
| Hash PAN | SHA-256 + HMAC-SHA256 with env secret → `card_hash`. Raw PAN **never** leaves gateway |
| Generate `txn_id` | UUID v4 |
| Idempotency check | Redis key: `idempotency:{txn_id}` — TTL 24 h. Duplicate → return `ALREADY_EXISTS` |
| Enrich event | Add `txn_id`, `timestamp`, `status: RECEIVED` |
| Publish to Kafka | Topic: `transactions`, partition key: `card_hash` |
| Return ACK | `{ txn_id, status: PENDING }` immediately — non-blocking |

Client can poll `GetTransactionStatus(txn_id)` for the fraud result.

---

### Step 2 — Kafka (transactions topic)

| Setting | Value |
|---|---|
| Partitions | 12 (handles 5K TPS at ≤500 msg/s per partition) |
| Replication | 3 (survives 1 broker loss) |
| Partition key | `card_hash` — all txns for same card go in order |
| Retention | 3 days |
| Compression | lz4 |
| Producer | `acks=all`, `enable.idempotence=true` — no data loss |

---

### Step 3 — Fraud Engine

**3.1 Rule Engine (synchronous, in-memory)**

| Rule ID | What it checks | Score Contribution |
|---|---|---|
| HIGH_AMOUNT | Amount > threshold per currency (e.g. >500 OMR, >50K INR) | 200 |
| ROUND_AMOUNT | Suspiciously round large amount (structuring attack) | 80 |
| RISKY_MCC | Gambling, ATM cash, wire transfer, crypto MCCs | 150 |
| CRYPTO_EXCHANGE_MCC | Cryptocurrency exchange MCCs | 120 |
| NIGHT_TRANSACTION | 00:00–05:00 UTC (covers Gulf midnight–3am) | 60 |
| WEEKEND_HIGH_AMOUNT | Weekend + HIGH_AMOUNT both triggered | 40 |
| CARD_NOT_PRESENT | Online / PG channel | 50 |
| FOREIGN_CURRENCY | Currency not in domestic list (OMR/AED/SAR/BHD/KWD/QAR) | 70 |

Triggered rules → added to `triggered_rules[]`

**3.2 Velocity Engine (Redis sorted sets)**

| Rule ID | Window | Threshold | Score |
|---|---|---|---|
| TXN_COUNT_1MIN | 60 s | 5 txns | 250 |
| TXN_COUNT_5MIN | 5 min | 10 txns | 150 |
| TXN_COUNT_1HR | 60 min | 20 txns | 80 |
| AMOUNT_SUM_1MIN | 60 s | 100 OMR equivalent | 200 |
| AMOUNT_SUM_1HR | 60 min | 1000 OMR equivalent | 100 |
| UNIQUE_MERCHANTS_5MIN | 5 min | 3 distinct merchants | 180 |
| UNIQUE_TERMINALS_5MIN | 5 min | 4 distinct terminals | 120 |

Redis key pattern: `velocity:count:{rule_id}:{card_hash}` (count-based rules) / `velocity:amount:{rule_id}:{card_hash}` (amount-based rules)
Structure: sorted set (score = Unix timestamp, member = txn_id)

**3.3 Feature Preparation**

Build feature vector for Risk Service:

```json
{
  "amount_minor": 5000000,
  "txn_count_1min": 5,
  "txn_count_5min": 8,
  "txn_count_1hr": 12,
  "amount_sum_1min": 5000000,
  "amount_sum_1hr": 9500000,
  "unique_merchants_5min": 3,
  "merchant_risk": 0.8,
  "is_night": 0,
  "is_risky_mcc": 1,
  "is_card_not_present": 0
}
```

Merchant risk scores (0.0–1.0) loaded from DB → Redis at startup (`merchant_risk:{merchant_id}`).
Default for unknown merchants: 0.5.

**3.4 gRPC call to Risk Service**

```
RiskRequest {
  txn_id, card_hash, amount_minor, currency,
  merchant_id, mcc,
  velocity_window, velocity_count, velocity_amount,
  transaction_type, channel
}
```

---

### Step 4 — Risk Service

Receives feature vector, applies weighted model:

```
risk_score = (amount_weight × amount)
           + (velocity_weight × txn_count)
           + (merchant_weight × merchant_risk)
           + ...
```

Returns:

```json
{
  "risk_score": 742,
  "decision": "DECLINE",
  "triggered_rules": ["HIGH_AMOUNT", "RISKY_MCC"]
}
```

Score range: **100 (lowest) → 1000 (highest risk)**

---

### Step 5 — Fraud Engine: Final Decision

Merges rule results + risk score:

| Score Range | Decision |
|---|---|
| < 400 | APPROVE |
| 400 – 700 | REVIEW |
| > 700 | DECLINE |

Publishes `FraudResultEvent` to Kafka topic: `fraud_results`

```json
{
  "txn_id": "abc-123",
  "card_hash": "sha256hash",
  "risk_score": 742,
  "decision": "DECLINE",
  "triggered_rules": ["HIGH_AMOUNT", "RISKY_MCC"],
  "decided_at": "2025-08-15T09:31:00.456Z"
}
```

---

### Step 6 — Persistence Service

Consumes from `fraud_results`. Upserts into PostgreSQL:

```sql
INSERT INTO transactions (...) VALUES (...)
ON CONFLICT (txn_id, txn_timestamp)
DO UPDATE SET status, risk_score, triggered_rules, processed_at, updated_at;
```

**PostgreSQL schema highlights:**
- Amount stored as `BIGINT` minor units (avoids float rounding — e.g. OMR 5.250 → 5250)
- Raw PAN never stored — only `card_hash` (SHA-256) + `masked_pan` (411111XXXXXX1111)
- Table partitioned by `txn_timestamp` (monthly partitions, 2025–2026 pre-created)
- Indexes: `card_hash`, `txn_timestamp`, `card_hash + txn_timestamp`, `rrn`, `status`
- `updated_at` maintained via trigger `trg_transactions_updated_at`

---

### Step 7 — Metrics

Each service exposes `/metrics` → scraped by Prometheus → dashboards in Grafana.

Key metrics:
- `sentinel_idempotency_redis_miss_total` — Redis down events at gateway
- `sentinel_redis_health{status}` — Redis health per service
- Fraud decision distribution (APPROVE / REVIEW / DECLINE counts)
- Rule trigger rates per rule ID
- Risk score histogram
- Kafka consumer lag per consumer group

---

## 4. Failure Scenarios

### Case 1 — Fraud Engine Crash
Kafka retains the message (3-day retention). Another consumer instance in `fraud-engine-cg` picks it up. No data loss.

### Case 2 — Risk Service Down
Circuit breaker in Fraud Engine:
- Opens after **5 consecutive failures**
- gRPC timeout: **500 ms**
- Fallback score: **600** → Decision: **REVIEW** (never auto-DECLINE on infra failure)
- Circuit stays open for **15 s**, then probes with half-open
- State shared across all FE replicas via Redis (`cb:risk_service:state`)
- **Observability on state transition:** structured JSON log emitted with fields `circuit_state`, `consecutive_failures`, `fallback_used`, `risk_service_error`
- **Prometheus metrics:** `sentinel_cb_state_transitions_total` (labels: `from_state`, `to_state`), `sentinel_cb_fallback_decisions_total`, `sentinel_cb_open_duration_seconds`
- **Alert:** fires at severity=critical if circuit remains OPEN > 30 s (see `config/fraud-rules.yaml` → `circuit_breaker.alerting`)

### Case 3 — PostgreSQL Down
Persistence Service retries with exponential backoff (3 attempts, max 30 s total).
On 3rd failure → publishes to **`transaction_dlq`** with headers:

| Header | Value |
|---|---|
| `dlq-original-topic` | `fraud_results` |
| `dlq-original-offset` | `<offset>` |
| `dlq-failure-reason` | `<exception class>` |
| `dlq-retry-count` | `3` |
| `dlq-failed-at` | ISO 8601 UTC |

DLQ retention: **14 days**. Ops team replays via `dlq-retry-worker-cg`.

### Case 4 — Duplicate Request
API Gateway checks Redis idempotency key before publishing to Kafka.
Duplicate within 24 h → returns original `TransactionAck` (gRPC `ALREADY_EXISTS`).

---

## 5. Kafka Topics Summary

| Topic | Partitions | Retention | Producer | Consumer(s) |
|---|---|---|---|---|
| `transactions` | 12 | 3 days | API Gateway | Fraud Engine (`fraud-engine-cg`) |
| `fraud_results` | 12 | 7 days | Fraud Engine | Persistence Svc (`persistence-svc-cg`), Ops Dashboard (`ops-dashboard-cg`) |
| `transaction_dlq` | 3 | 14 days | Persistence Svc | DLQ retry worker (`dlq-retry-worker-cg`) |

---

## 6. Redis Usage Summary

| Use Case | Key Pattern | TTL | Structure |
|---|---|---|---|
| Idempotency | `idempotency:{txn_id}` | 24 h | String |
| Velocity count | `velocity:count:{rule_id}:{card_hash}` | window + 60 s | Sorted Set |
| Velocity HLL | `hll:merchants:{card_hash}:{bucket}` | window | HyperLogLog |
| Merchant risk | `merchant_risk:{merchant_id}` | 24 h | String (float) |
| Circuit breaker | `cb:risk_service:state` | 60 s | String |

Redis databases: DB0 = velocity + merchant risk, DB1 = idempotency, DB2 = circuit breaker state.

Eviction policy (per DB — strongly prefer separate Redis instances):
| DB | Use-case | Policy | Rationale |
|----|----------|--------|-----------|
| 0 | Velocity counters | `allkeys-lru` | All keys carry TTLs; LRU eviction under pressure is safe |
| 1 | Idempotency | `noeviction` | Silent eviction = duplicate processing = double charge. API Gateway must treat Redis OOM as 503. Capacity-alarm at 80 % memory. |
| 2 | Circuit breaker | `volatile-lru` | CB state keys carry TTLs; volatile-lru only evicts keyed items |

---

## 7. Proto / Contract Summary

| File | Package | Purpose |
|---|---|---|
| `proto/gateway.proto` | `sentinel.gateway.v1` | Client → API Gateway (SubmitTransaction, GetTransactionStatus) |
| `proto/transactions.proto` | `sentinel.transactions.v1` | Kafka message: TransactionEvent (API GW → Fraud Engine) |
| `proto/risk.proto` | `sentinel.risk.v1` | Fraud Engine → Risk Service gRPC (CalculateRisk) |
| `proto/fraud_results.proto` | `sentinel.fraudresults.v1` | Kafka message: FraudResultEvent (Fraud Engine → Persistence Svc) |

**Key design decisions in protos:**
- `amount` stored as `int64 amount_minor` everywhere — avoids float rounding errors (e.g. OMR has 3 decimal places)
- Raw PAN never in any proto — only `card_hash` + `pan_last4`
- `merchant_id` derived from JWT at gateway — clients cannot forge it
- Each proto package is self-contained (enums redeclared per package — schema registry independence)

---

## 8. What Has Been Built (Files Created)

| File | Status | What it defines |
|---|---|---|
| [config/kafka-topics.yaml](config/kafka-topics.yaml) | Done | 3 topics, partitions, retention, consumer group settings, producer defaults |
| [config/redis.yaml](config/redis.yaml) | Done | Connection (cluster + standalone), idempotency, velocity, merchant risk, circuit breaker, memory sizing |
| [config/fraud-rules.yaml](config/fraud-rules.yaml) | Done | 8 rules, 7 velocity checks, decision thresholds, feature vector definition, circuit breaker config |
| [config/schema-registry.yaml](config/schema-registry.yaml) | Done | Schema registry config |
| [proto/gateway.proto](proto/gateway.proto) | Done | GatewayService gRPC — SubmitTransaction, GetTransactionStatus |
| [proto/transactions.proto](proto/transactions.proto) | Done | TransactionEvent Kafka message schema |
| [proto/risk.proto](proto/risk.proto) | Done | RiskService gRPC — CalculateRisk request/response |
| [proto/fraud_results.proto](proto/fraud_results.proto) | Done | FraudResultEvent Kafka message schema |
| [db/migrations/001_create_transactions.sql](db/migrations/001_create_transactions.sql) | Done | PostgreSQL schema — partitioned table, indexes, upsert template, updated_at trigger |

---

## 9. What Remains

| Layer | What needs to be built |
|---|---|
| Service configs | `api-gateway.yaml`, `fraud-engine.yaml`, `persistence-svc.yaml`, `risk-service.yaml` |
| Docker Compose | Full local stack (Kafka, ZooKeeper/KRaft, Redis Cluster, PostgreSQL, all services) |
| Service code | API Gateway, Fraud Engine, Risk Service, Persistence Service (language TBD) |
| Prometheus config | `prometheus.yml` — scrape targets per service |
| Grafana dashboards | Fraud decision rates, rule trigger rates, risk score histogram, consumer lag |
| Tests | Unit (rule engine logic), integration (full Kafka → FE → DB flow), failure scenarios |
| DB migrations 002+ | Any schema evolution (indexes, new columns) |
