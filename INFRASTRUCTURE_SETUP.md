# SentinelSwitch — Infrastructure Setup Guide

**Ticket:** CREDO-ALERT-001
**Scope:** Kafka, Redis, PostgreSQL, Schema Registry, Prometheus, Grafana — local Docker Compose and production notes

---

## Table of Contents

1. [Overview](#1-overview)
2. [Prerequisites](#2-prerequisites)
3. [Docker Compose — Full Stack](#3-docker-compose--full-stack)
4. [Kafka Setup](#4-kafka-setup)
5. [Redis Setup](#5-redis-setup)
6. [PostgreSQL Setup](#6-postgresql-setup)
7. [Schema Registry Setup](#7-schema-registry-setup)
8. [Prometheus Setup](#8-prometheus-setup)
9. [Grafana Setup](#9-grafana-setup)
10. [Environment Variables Reference](#10-environment-variables-reference)
11. [Production Notes](#11-production-notes)
12. [Health Check Verification](#12-health-check-verification)

---

## 1. Overview

SentinelSwitch depends on the following infrastructure components:

| Component | Local Port | Purpose |
|---|---|---|
| Kafka Broker | 9092 | Transaction and result event streaming |
| Zookeeper | 2181 | Kafka coordination |
| Schema Registry | 8081 | Protobuf schema management |
| Redis | 6379 | Idempotency, velocity counters, circuit breaker state |
| PostgreSQL | 5432 | Persistent transaction + fraud result store |
| Prometheus | 9090 | Metrics scraping from all services |
| Grafana | 3000 | Metrics dashboards |

All four SentinelSwitch services connect outward to these components. None of the infrastructure components call the services.

---

## 2. Prerequisites

| Tool | Minimum Version | Install |
|---|---|---|
| Docker | 24.x | https://docs.docker.com/get-docker/ |
| Docker Compose | v2.x (plugin) | Included with Docker Desktop |
| `kafka-topics` CLI | Kafka 3.x | `brew install kafka` or via Confluent CLI |
| `psql` | 14.x | `brew install postgresql` |
| `redis-cli` | 7.x | `brew install redis` |
| `buf` | 1.x | https://buf.build/docs/installation |

Verify your setup:

```bash
docker --version          # Docker version 24.x.x
docker compose version    # Docker Compose version v2.x.x
```

---

## 3. Docker Compose — Full Stack

Create `docker-compose.yml` at the repository root:

```yaml
version: "3.9"

services:

  # ---------------------------------------------------------------------------
  # Zookeeper — Kafka coordination
  # ---------------------------------------------------------------------------
  zookeeper:
    image: confluentinc/cp-zookeeper:7.5.0
    hostname: zookeeper
    container_name: sentinel-zookeeper
    ports:
      - "2181:2181"
    environment:
      ZOOKEEPER_CLIENT_PORT: 2181
      ZOOKEEPER_TICK_TIME: 2000
    healthcheck:
      test: ["CMD", "bash", "-c", "echo ruok | nc localhost 2181 | grep imok"]
      interval: 10s
      timeout: 5s
      retries: 5

  # ---------------------------------------------------------------------------
  # Kafka — single broker (local dev)
  # Production: 3+ brokers; use Confluent Cloud or self-hosted cluster
  # ---------------------------------------------------------------------------
  kafka:
    image: confluentinc/cp-kafka:7.5.0
    hostname: kafka
    container_name: sentinel-kafka
    depends_on:
      zookeeper:
        condition: service_healthy
    ports:
      - "9092:9092"
      - "9093:9093"
    environment:
      KAFKA_BROKER_ID: 1
      KAFKA_ZOOKEEPER_CONNECT: zookeeper:2181
      # Listener for Docker-internal traffic (service-to-service)
      KAFKA_LISTENERS: INTERNAL://0.0.0.0:9093,EXTERNAL://0.0.0.0:9092
      KAFKA_ADVERTISED_LISTENERS: INTERNAL://kafka:9093,EXTERNAL://localhost:9092
      KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: INTERNAL:PLAINTEXT,EXTERNAL:PLAINTEXT
      KAFKA_INTER_BROKER_LISTENER_NAME: INTERNAL
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: 1        # single broker → 1
      KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR: 1
      KAFKA_TRANSACTION_STATE_LOG_MIN_ISR: 1
      KAFKA_AUTO_CREATE_TOPICS_ENABLE: "false"         # topics created explicitly
      KAFKA_LOG_RETENTION_HOURS: 72
      KAFKA_LOG_SEGMENT_BYTES: 1073741824
      KAFKA_NUM_PARTITIONS: 12
    healthcheck:
      test: ["CMD", "kafka-broker-api-versions", "--bootstrap-server", "localhost:9092"]
      interval: 15s
      timeout: 10s
      retries: 10

  # ---------------------------------------------------------------------------
  # Confluent Schema Registry
  # ---------------------------------------------------------------------------
  schema-registry:
    image: confluentinc/cp-schema-registry:7.5.0
    hostname: schema-registry
    container_name: sentinel-schema-registry
    depends_on:
      kafka:
        condition: service_healthy
    ports:
      - "8081:8081"
    environment:
      SCHEMA_REGISTRY_HOST_NAME: schema-registry
      SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS: kafka:9093
      SCHEMA_REGISTRY_LISTENERS: http://0.0.0.0:8081
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8081/subjects"]
      interval: 10s
      timeout: 5s
      retries: 10

  # ---------------------------------------------------------------------------
  # Redis — standalone (local dev; production: Redis Cluster 6+ nodes)
  # ---------------------------------------------------------------------------
  redis:
    image: redis:7.2-alpine
    hostname: redis
    container_name: sentinel-redis
    ports:
      - "6379:6379"
    command: >
      redis-server
      --maxmemory 2gb
      --maxmemory-policy noeviction
      --appendonly no
      --save ""
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10

  # ---------------------------------------------------------------------------
  # PostgreSQL — partitioned transactions table
  # ---------------------------------------------------------------------------
  postgres:
    image: postgres:16-alpine
    hostname: postgres
    container_name: sentinel-postgres
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: sentinelswitch
      POSTGRES_USER: sentinel
      POSTGRES_PASSWORD: sentinel_local_secret
    volumes:
      - sentinel-postgres-data:/var/lib/postgresql/data
      - ./db/migrations:/docker-entrypoint-initdb.d:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U sentinel -d sentinelswitch"]
      interval: 10s
      timeout: 5s
      retries: 10

  # ---------------------------------------------------------------------------
  # Prometheus — scrapes /metrics from all services
  # ---------------------------------------------------------------------------
  prometheus:
    image: prom/prometheus:v2.48.1
    hostname: prometheus
    container_name: sentinel-prometheus
    ports:
      - "9090:9090"
    volumes:
      - ./infra/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - sentinel-prometheus-data:/prometheus
    command:
      - "--config.file=/etc/prometheus/prometheus.yml"
      - "--storage.tsdb.path=/prometheus"
      - "--storage.tsdb.retention.time=15d"
      - "--web.enable-lifecycle"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:9090/-/healthy"]
      interval: 15s
      timeout: 5s
      retries: 5

  # ---------------------------------------------------------------------------
  # Grafana — fraud dashboards
  # ---------------------------------------------------------------------------
  grafana:
    image: grafana/grafana:10.2.2
    hostname: grafana
    container_name: sentinel-grafana
    depends_on:
      - prometheus
    ports:
      - "3000:3000"
    environment:
      GF_SECURITY_ADMIN_USER: admin
      GF_SECURITY_ADMIN_PASSWORD: sentinel_grafana
      GF_AUTH_ANONYMOUS_ENABLED: "false"
    volumes:
      - sentinel-grafana-data:/var/lib/grafana
      - ./infra/grafana/provisioning:/etc/grafana/provisioning:ro

volumes:
  sentinel-postgres-data:
  sentinel-prometheus-data:
  sentinel-grafana-data:
```

Start all infrastructure:

```bash
docker compose up -d zookeeper kafka schema-registry redis postgres prometheus grafana

# Wait for all health checks to pass
docker compose ps
```

All services should show `healthy` before proceeding.

---

## 4. Kafka Setup

### 4.1 Create Topics

After Kafka is healthy, create the three topics. Run from inside the container or with the local Kafka CLI:

```bash
BROKER=localhost:9092

# --- transactions ---
kafka-topics --bootstrap-server $BROKER \
  --create --topic transactions \
  --partitions 12 --replication-factor 1 \
  --config retention.ms=259200000 \
  --config retention.bytes=53687091200 \
  --config cleanup.policy=delete \
  --config min.insync.replicas=1 \
  --config max.message.bytes=1048576 \
  --config compression.type=lz4

# --- fraud_results ---
kafka-topics --bootstrap-server $BROKER \
  --create --topic fraud_results \
  --partitions 12 --replication-factor 1 \
  --config retention.ms=604800000 \
  --config retention.bytes=21474836480 \
  --config cleanup.policy=delete \
  --config min.insync.replicas=1 \
  --config max.message.bytes=1048576 \
  --config compression.type=lz4

# --- transaction_dlq ---
kafka-topics --bootstrap-server $BROKER \
  --create --topic transaction_dlq \
  --partitions 3 --replication-factor 1 \
  --config retention.ms=1209600000 \
  --config retention.bytes=1073741824 \
  --config cleanup.policy=delete \
  --config min.insync.replicas=1 \
  --config max.message.bytes=1048576 \
  --config compression.type=lz4
```

> **Production note:** Use `--replication-factor 3` and `--config min.insync.replicas=2` on a 3-broker cluster.

### 4.2 Verify Topics

```bash
kafka-topics --bootstrap-server $BROKER --list
# Expected: fraud_results  transaction_dlq  transactions

kafka-topics --bootstrap-server $BROKER --describe --topic transactions
kafka-topics --bootstrap-server $BROKER --describe --topic fraud_results
kafka-topics --bootstrap-server $BROKER --describe --topic transaction_dlq
```

### 4.3 Consumer Group Reference

| Consumer Group | Topic | Service |
|---|---|---|
| `fraud-engine-cg` | `transactions` | Fraud Engine |
| `persistence-svc-cg` | `fraud_results` | Persistence Service |
| `ops-dashboard-cg` | `fraud_results` | Ops Dashboard (future) |
| `dlq-retry-worker-cg` | `transaction_dlq` | DLQ Retry Worker (future) |

Check consumer lag after services start:

```bash
kafka-consumer-groups --bootstrap-server $BROKER \
  --group fraud-engine-cg --describe

kafka-consumer-groups --bootstrap-server $BROKER \
  --group persistence-svc-cg --describe
```

---

## 5. Redis Setup

### 5.1 Local Dev — Single Instance

The Docker Compose above starts a single Redis instance on port 6379 with `noeviction` policy (safest default for local dev).

Verify:

```bash
redis-cli -h localhost -p 6379 ping
# PONG
```

### 5.2 Database Assignments

| DB | Used By | Key Patterns | Eviction Policy |
|---|---|---|---|
| DB 0 | Fraud Engine | `velocity:count:*`, `velocity:amount:*`, `hll:merchants:*`, `hll:terminals:*`, `merchant_risk:*` | `allkeys-lru` |
| DB 1 | API Gateway | `idempotency:*` | `noeviction` (CRITICAL) |
| DB 2 | Fraud Engine | `cb:risk_service:state`, `cb:risk_service:failures` | `volatile-lru` |

Switch between databases:

```bash
redis-cli -h localhost -p 6379 -n 0 keys 'velocity:*'
redis-cli -h localhost -p 6379 -n 1 keys 'idempotency:*'
redis-cli -h localhost -p 6379 -n 2 get cb:risk_service:state
```

### 5.3 Verify Key Behaviour

After sending a test transaction through the API Gateway:

```bash
# Idempotency key should appear
redis-cli -n 1 keys 'idempotency:*'

# Velocity sorted sets should appear
redis-cli -n 0 keys 'velocity:count:TXN_COUNT_1MIN:*'

# Circuit breaker state (starts CLOSED)
redis-cli -n 2 get cb:risk_service:state
# (nil) or "CLOSED"
```

### 5.4 Memory Sizing (Production)

| Use Case | Estimated Memory at 5K TPS |
|---|---|
| Idempotency (24h, 432M keys) | ~34 GB — requires Redis Cluster |
| Velocity sorted sets (active cards) | 5–10 GB |
| Merchant risk (100K merchants) | ~5 MB |
| Circuit breaker state | < 1 KB |

Production recommendation: three separate Redis instances (or cluster namespaces) — one per DB to allow independent sizing and eviction policy.

### 5.5 Circuit Breaker Key Reference

| Key | Value | TTL |
|---|---|---|
| `cb:risk_service:state` | `CLOSED` \| `OPEN` \| `HALF_OPEN` | 60 s (OPEN auto-expires → HALF_OPEN) |
| `cb:risk_service:failures` | integer (0–N) | 30 s (resets after 30 s of no errors) |

---

## 6. PostgreSQL Setup

### 6.1 Migration

The Docker Compose volume mounts `./db/migrations/` into `/docker-entrypoint-initdb.d/`. PostgreSQL runs all `.sql` files there on first startup. Migration `001_create_transactions.sql` is applied automatically.

If you need to apply manually:

```bash
psql -h localhost -p 5432 -U sentinel -d sentinelswitch \
  -f db/migrations/001_create_transactions.sql
```

### 6.2 Verify Table and Partitions

```bash
psql -h localhost -p 5432 -U sentinel -d sentinelswitch
```

```sql
-- Confirm table exists and is partitioned
\d transactions

-- List all partitions (expect 24 monthly + 1 default)
SELECT tablename
FROM   pg_tables
WHERE  tablename LIKE 'transactions_%'
ORDER  BY tablename;

-- Confirm indexes
\di transactions*
```

Expected partitions: `transactions_2025_01` … `transactions_2025_12`, `transactions_2026_01` … `transactions_2026_12`, `transactions_default`.

Expected indexes:
- `idx_txn_card_hash`
- `idx_txn_timestamp`
- `idx_txn_rrn`
- `idx_txn_status`
- `idx_txn_card_time`

### 6.3 Test Upsert

```sql
INSERT INTO transactions (
    txn_id, card_hash, masked_pan, amount_minor, currency,
    merchant_id, transaction_type, channel, scheme, rrn,
    terminal_id, mcc, txn_timestamp,
    status, risk_score, triggered_rules, processed_at
) VALUES (
    gen_random_uuid(),
    'aabbcc0011223344aabbcc0011223344aabbcc0011223344aabbcc0011223344',
    '411111XXXXXX1111',
    500000,
    'OMR',
    'M00000000000123',
    'SALES',
    'POS',
    'VISA',
    '123456789012',
    'T0000456',
    '5411',
    NOW(),
    'APPROVED',
    350,
    '[]'::jsonb,
    NOW()
)
ON CONFLICT (txn_id, txn_timestamp)
DO UPDATE SET
    status          = EXCLUDED.status,
    risk_score      = EXCLUDED.risk_score,
    triggered_rules = EXCLUDED.triggered_rules,
    processed_at    = EXCLUDED.processed_at,
    updated_at      = NOW();

-- Confirm row landed in correct partition
SELECT tableoid::regclass AS partition, txn_id, status, risk_score
FROM   transactions;
```

### 6.4 Connection Pool Reference

| Setting | Value |
|---|---|
| `max_open_conns` | 20 |
| `max_idle_conns` | 5 |
| `conn_max_lifetime` | 1800 s (30 min) |
| `conn_max_idle_time` | 300 s (5 min) |
| `connect_timeout_ms` | 5000 ms |

### 6.5 Merchants Table (for warm-up)

The Fraud Engine warm-up loads merchant risk scores from PostgreSQL at startup:

```sql
-- Create a minimal merchants table for local dev
CREATE TABLE IF NOT EXISTS merchants (
    merchant_id  VARCHAR(50) PRIMARY KEY,
    risk_score   NUMERIC(4,3) NOT NULL DEFAULT 0.5
                 CHECK (risk_score BETWEEN 0.0 AND 1.0),
    updated_at   TIMESTAMP    NOT NULL DEFAULT NOW()
);

-- Seed a few merchants
INSERT INTO merchants (merchant_id, risk_score) VALUES
  ('M00000000000123', 0.2),
  ('M00000000000456', 0.8),
  ('M00000000000789', 0.5)
ON CONFLICT (merchant_id) DO NOTHING;
```

---

## 7. Schema Registry Setup

### 7.1 Register Protobuf Schemas

After `schema-registry` is healthy and `buf generate` has been run (see [buf.gen.yaml](buf.gen.yaml)), register the two subjects used by Kafka producers:

```bash
SR=http://localhost:8081

# Register transactions-value (TransactionEvent)
curl -X POST "$SR/subjects/transactions-value/versions" \
  -H "Content-Type: application/vnd.schemaregistry.v1+json" \
  --data-binary "{\"schemaType\": \"PROTOBUF\", \"schema\": $(cat proto/transactions.proto | jq -Rs .)}"

# Register fraud-results-value (FraudResultEvent)
curl -X POST "$SR/subjects/fraud-results-value/versions" \
  -H "Content-Type: application/vnd.schemaregistry.v1+json" \
  --data-binary "{\"schemaType\": \"PROTOBUF\", \"schema\": $(cat proto/fraud_results.proto | jq -Rs .)}"
```

### 7.2 Verify Schemas

```bash
# List all registered subjects
curl -s http://localhost:8081/subjects | jq .
# ["fraud-results-value","transactions-value"]

# Check latest version of transactions-value
curl -s http://localhost:8081/subjects/transactions-value/versions/latest | jq .
```

### 7.3 Compatibility Mode

Both subjects use `FULL` compatibility by default. The Schema Registry rejects schema changes that would break existing producers or consumers.

To check or set compatibility:

```bash
# Check
curl -s http://localhost:8081/config/transactions-value

# Set to FULL (recommended)
curl -X PUT http://localhost:8081/config/transactions-value \
  -H "Content-Type: application/vnd.schemaregistry.v1+json" \
  -d '{"compatibility": "FULL"}'
```

---

## 8. Prometheus Setup

### 8.1 Prometheus Config

Create `infra/prometheus/prometheus.yml`:

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:

  - job_name: api-gateway
    static_configs:
      - targets: ["host.docker.internal:9091"]
    metrics_path: /metrics

  - job_name: fraud-engine
    static_configs:
      - targets: ["host.docker.internal:9095"]
    metrics_path: /metrics

  - job_name: risk-service
    static_configs:
      - targets: ["host.docker.internal:9094"]
    metrics_path: /metrics

  - job_name: persistence-svc
    static_configs:
      - targets: ["host.docker.internal:9093"]
    metrics_path: /metrics
```

> **Note:** `host.docker.internal` resolves to the host machine from inside Docker. Use actual service hostnames when running services inside Docker Compose.

### 8.2 Service Metrics Ports

| Service | Metrics Port | Endpoint |
|---|---|---|
| API Gateway | 9091 | `http://localhost:9091/metrics` |
| Persistence Service | 9093 | `http://localhost:9093/metrics` |
| Risk Service | 9094 | `http://localhost:9094/metrics` |
| Fraud Engine | 9095 | `http://localhost:9095/metrics` |

### 8.3 Key Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `sentinel_idempotency_redis_miss_total` | Counter | — | Redis unavailable at API Gateway; allow-through fired |
| `sentinel_cb_state_transitions_total` | Counter | `from_state`, `to_state` | Circuit breaker state changes |
| `sentinel_cb_fallback_decisions_total` | Counter | — | Fallback REVIEW issued (Risk Service unreachable) |
| `sentinel_cb_open_duration_seconds` | Gauge | — | Time circuit has been OPEN |
| `sentinel_risk_score_histogram` | Histogram | `decision` | Risk score distribution per decision |
| `sentinel_persistence_upserts_total` | Counter | `status` | DB write outcomes: `success` \| `retry` \| `dlq` |
| `sentinel_persistence_dlq_publishes_total` | Counter | — | Messages sent to DLQ |
| `sentinel_redis_health` | Gauge | `status` | Redis connectivity: `up` or `down` |

Verify Prometheus is scraping after services start:

```bash
curl -s http://localhost:9090/api/v1/targets | jq '.data.activeTargets[].health'
# "up" for each target
```

---

## 9. Grafana Setup

### 9.1 Access

```
URL:      http://localhost:3000
Username: admin
Password: sentinel_grafana
```

### 9.2 Provision Prometheus Data Source

Create `infra/grafana/provisioning/datasources/prometheus.yml`:

```yaml
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    url: http://prometheus:9090
    access: proxy
    isDefault: true
```

### 9.3 Key Dashboard Panels

Create a dashboard with these panels to monitor SentinelSwitch:

**Row 1 — Throughput**
- `rate(sentinel_persistence_upserts_total{status="success"}[1m])` — transactions/s persisted
- `rate(sentinel_risk_requests_total{status="success"}[1m])` — risk scores/s

**Row 2 — Decision Distribution**
- `sentinel_risk_score_histogram_bucket` — histogram of risk scores
- Pie chart: `sentinel_persistence_upserts_total` grouped by label for APPROVE/REVIEW/DECLINE

**Row 3 — Circuit Breaker**
- `sentinel_cb_open_duration_seconds` — gauge: 0 is healthy
- `rate(sentinel_cb_fallback_decisions_total[5m])` — fallbacks per minute
- `sentinel_cb_state_transitions_total` — state change events

**Row 4 — DLQ and Errors**
- `rate(sentinel_persistence_dlq_publishes_total[5m])` — DLQ rate
- `sentinel_idempotency_redis_miss_total` — cumulative Redis misses

**Recommended Alerts:**
- Circuit breaker OPEN > 30 s → `sentinel_cb_open_duration_seconds > 30`
- DLQ rate > 0 for 2 minutes → `rate(sentinel_persistence_dlq_publishes_total[2m]) > 0`
- Redis down → `sentinel_redis_health{status="down"} == 1`

---

## 10. Environment Variables Reference

All services read these environment variables. Set them in `.env` files or your orchestration secrets store.

### Shared

| Variable | Default | Required | Purpose |
|---|---|---|---|
| `KAFKA_BROKERS` | `localhost:9092` | Yes | Comma-separated Kafka broker list |
| `SCHEMA_REGISTRY_URL` | `http://localhost:8081` | Yes | Confluent Schema Registry URL |
| `LOG_LEVEL` | `info` | No | `debug` \| `info` \| `warn` \| `error` |
| `DEPLOY_ENV` | `local` | No | Environment tag in logs |
| `SERVICE_VERSION` | `unknown` | No | Version tag in logs |

### API Gateway

| Variable | Default | Required | Purpose |
|---|---|---|---|
| `PAN_HASH_SECRET` | — | **YES** | HMAC-SHA256 secret for PAN hashing; never hardcode |
| `REDIS_HOST` | `localhost` | Yes | Redis hostname (standalone mode) |
| `REDIS_PORT` | `6379` | No | Redis port |
| `REDIS_PASSWORD` | — | Prod only | Redis auth password |
| `REDIS_TLS` | `false` | Prod only | Enable TLS to Redis |
| `GATEWAY_GRPC_PORT` | `50051` | No | gRPC listener port |
| `GATEWAY_METRICS_PORT` | `9091` | No | Prometheus metrics port |
| `GATEWAY_HEALTH_PORT` | `8081` | No | Health check HTTP port |
| `RATE_LIMITING_ENABLED` | `true` | No | Toggle token-bucket rate limiter |

### Fraud Engine

| Variable | Default | Required | Purpose |
|---|---|---|---|
| `RISK_SERVICE_HOST` | `risk-service` | Yes | Risk Service hostname |
| `RISK_SERVICE_PORT` | `50052` | No | Risk Service gRPC port |
| `RULES_CONFIG_FILE` | `config/fraud-rules.yaml` | No | Path to rules config |
| `FRAUD_ENGINE_WORKERS` | `100` | No | Worker pool size |
| `FRAUD_ENGINE_METRICS_PORT` | `9095` | No | Prometheus metrics port |
| `FRAUD_ENGINE_HEALTH_PORT` | `8082` | No | Health check HTTP port |

### Risk Service

| Variable | Default | Required | Purpose |
|---|---|---|---|
| `RISK_SERVICE_GRPC_PORT` | `50052` | No | gRPC listener port |
| `RISK_SERVICE_METRICS_PORT` | `9094` | No | Prometheus metrics port |
| `RISK_SERVICE_HEALTH_PORT` | `8084` | No | Health check HTTP port |
| `RISK_SERVICE_MAX_STREAMS` | `500` | No | Max concurrent gRPC streams |
| `RISK_SERVICE_WORKERS` | `0` (= GOMAXPROCS) | No | Scoring goroutine pool size |

### Persistence Service

| Variable | Default | Required | Purpose |
|---|---|---|---|
| `POSTGRES_HOST` | `localhost` | Yes | PostgreSQL hostname |
| `POSTGRES_PORT` | `5432` | No | PostgreSQL port |
| `POSTGRES_DB` | `sentinelswitch` | No | Database name |
| `POSTGRES_USER` | `sentinel` | No | DB username |
| `POSTGRES_PASSWORD` | — | **YES** | DB password; never hardcode |
| `POSTGRES_MAX_OPEN` | `20` | No | Max open DB connections |
| `POSTGRES_MAX_IDLE` | `5` | No | Max idle DB connections |
| `PERSISTENCE_SVC_METRICS_PORT` | `9093` | No | Prometheus metrics port |
| `PERSISTENCE_SVC_HEALTH_PORT` | `8083` | No | Health check HTTP port |

---

## 11. Production Notes

### Kafka

- Use 3 brokers minimum with `replication_factor=3`, `min.insync.replicas=2`.
- Enable TLS between brokers and clients (`KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: INTERNAL:SSL,EXTERNAL:SSL`).
- Use Confluent Cloud or MSK for managed Kafka at scale. The topic settings in `config/kafka-topics.yaml` apply directly.
- Set `auto.create.topics.enable=false` on brokers — all topics must be created explicitly.

### Redis

- Use Redis Cluster with at least 6 nodes (3 primary, 3 replica) for high availability.
- Dedicate one cluster (or cluster namespace) per DB assignment to enforce eviction policies independently.
- DB 1 (idempotency) must use `maxmemory-policy noeviction`. Set a Prometheus alert at 80% memory to prevent OOM errors causing transaction rejection.
- Rotate `REDIS_PASSWORD` via your secrets manager (Vault, AWS Secrets Manager). Never commit plaintext credentials.

### PostgreSQL

- Use PostgreSQL 16+ with `pg_partman` for automated monthly partition creation.
- Create a read replica for reporting queries to avoid contention with the write path.
- The `updated_at` trigger requires PostgreSQL 14+ for row triggers on partitioned parent tables. On 10–13, create the trigger on each child partition.
- Enable connection pooling via PgBouncer between services and the database at high TPS.

### Schema Registry

- Enable FULL compatibility mode on all subjects before going to production.
- Never delete or re-create subjects — use schema evolution with reserved field numbers.
- Fields 7–9 in `risk.proto` are reserved (`velocity_window`, `velocity_count`, `velocity_amount`). Do not reuse those field numbers.

### Secrets Management

- `PAN_HASH_SECRET` and `POSTGRES_PASSWORD` must be injected at runtime from a secrets store (e.g. Vault, AWS Secrets Manager, Kubernetes Secrets).
- Never write secrets to `docker-compose.yml` or any config file committed to git.
- Rotate `PAN_HASH_SECRET` with a dual-key transition period — old hashes remain valid until expiry.

---

## 12. Health Check Verification

After starting all infrastructure and services, verify end-to-end health:

```bash
# Kafka — broker reachable
kafka-broker-api-versions --bootstrap-server localhost:9092

# Kafka — all topics exist
kafka-topics --bootstrap-server localhost:9092 --list

# Redis — responsive
redis-cli -h localhost ping

# Redis — idempotency DB is accessible
redis-cli -h localhost -n 1 ping

# PostgreSQL — table exists and partitions created
psql -h localhost -U sentinel -d sentinelswitch -c "\dt transactions*"

# Schema Registry — subjects registered
curl -s http://localhost:8081/subjects

# Prometheus — all targets up
curl -s http://localhost:9090/api/v1/targets | jq '.data.activeTargets[] | {job: .labels.job, health: .health}'

# Service health endpoints (after services start)
curl -s http://localhost:8081/healthz   # API Gateway
curl -s http://localhost:8082/healthz   # Fraud Engine
curl -s http://localhost:8083/healthz   # Persistence Service
curl -s http://localhost:8084/healthz   # Risk Service
```

All health endpoints return HTTP 200 with `{"status":"ok"}` when the service and its dependencies are ready.
