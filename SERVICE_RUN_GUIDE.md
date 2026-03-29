# SentinelSwitch — Service Run Guide

**Ticket:** CREDO-ALERT-001
**Scope:** How to build, configure, and run all four SentinelSwitch services locally and in Docker

---

## Table of Contents

1. [Repository Structure](#1-repository-structure)
2. [Prerequisites](#2-prerequisites)
3. [Step 0 — Start Infrastructure](#3-step-0--start-infrastructure)
4. [Step 1 — Generate Protobuf Code](#4-step-1--generate-protobuf-code)
5. [Step 2 — Risk Service](#5-step-2--risk-service)
6. [Step 3 — Fraud Engine](#6-step-3--fraud-engine)
7. [Step 4 — API Gateway](#7-step-4--api-gateway)
8. [Step 5 — Persistence Service](#8-step-5--persistence-service)
9. [Running All Services with Docker Compose](#9-running-all-services-with-docker-compose)
10. [Startup Order and Dependencies](#10-startup-order-and-dependencies)
11. [Environment File Reference](#11-environment-file-reference)
12. [Sending a Test Transaction](#12-sending-a-test-transaction)
13. [Observing the Pipeline](#13-observing-the-pipeline)
14. [Graceful Shutdown](#14-graceful-shutdown)
15. [Common Errors and Fixes](#15-common-errors-and-fixes)

---

## 1. Repository Structure

```
SentinelSwitch/
├── buf.yaml                        # buf.build proto root config
├── buf.gen.yaml                    # buf generate plugin config
├── proto/                          # .proto source files
│   ├── gateway/v1/gateway.proto   # GatewayService (API Gateway)
│   ├── risk/v1/risk.proto         # RiskService (Risk Service)
│   └── transactions/v1/...        # TransactionEvent, FraudResultEvent
├── services/
│   ├── proto-gen/                  # Generated Go code (buf generate output)
│   ├── api-gateway/
│   │   ├── cmd/main.go
│   │   ├── internal/
│   │   ├── Dockerfile
│   │   └── go.mod
│   ├── fraud-engine/
│   │   ├── cmd/main.go
│   │   ├── internal/
│   │   ├── Dockerfile
│   │   └── go.mod
│   ├── risk-service/
│   │   ├── cmd/main.go
│   │   ├── internal/
│   │   ├── Dockerfile
│   │   └── go.mod
│   └── persistence-svc/
│       ├── cmd/main.go
│       ├── internal/
│       ├── Dockerfile
│       └── go.mod
├── config/
│   ├── api-gateway.yaml
│   ├── fraud-engine.yaml
│   ├── risk-service.yaml
│   ├── persistence-svc.yaml
│   ├── fraud-rules.yaml
│   ├── kafka-topics.yaml
│   └── redis.yaml
└── db/
    └── migrations/
        └── 001_create_transactions.sql
```

All four services are Go 1.22 binaries with a single entrypoint: `./cmd/main.go`. Each service reads its YAML config file via the `CONFIG_PATH` environment variable (defaulting to `config/<service-name>.yaml`).

---

## 2. Prerequisites

| Tool | Version | Check |
|---|---|---|
| Go | 1.22+ | `go version` |
| buf | 1.x | `buf --version` |
| protoc-gen-go | latest | `protoc-gen-go --version` |
| protoc-gen-go-grpc | latest | `protoc-gen-go-grpc --version` |
| Docker + Compose | 24.x / v2.x | `docker compose version` |
| grpcurl (optional) | any | `grpcurl --version` |

Install proto toolchain:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/bufbuild/buf/cmd/buf@latest
```

Ensure `$GOPATH/bin` (usually `~/go/bin`) is on your `$PATH`.

---

## 3. Step 0 — Start Infrastructure

Before any service can run, Kafka, Redis, and PostgreSQL must be healthy. Follow [INFRASTRUCTURE_SETUP.md](INFRASTRUCTURE_SETUP.md) to start these components.

Minimum quick-start:

```bash
# Start required infrastructure
docker compose up -d zookeeper kafka schema-registry redis postgres

# Wait for all to be healthy (check every few seconds)
docker compose ps

# Create Kafka topics (required — auto-create is disabled)
BROKER=localhost:9092
kafka-topics --bootstrap-server $BROKER --create --topic transactions --partitions 12 --replication-factor 1
kafka-topics --bootstrap-server $BROKER --create --topic fraud_results --partitions 12 --replication-factor 1
kafka-topics --bootstrap-server $BROKER --create --topic transaction_dlq --partitions 3 --replication-factor 1

# Verify
kafka-topics --bootstrap-server $BROKER --list
```

---

## 4. Step 1 — Generate Protobuf Code

All four services import generated Go code from `services/proto-gen/`. This directory is populated by `buf generate` — it is not committed to source control and must be generated before building any service.

```bash
# Run from the repository root
buf generate

# Output lands in services/proto-gen/
# You should see files like:
#   services/proto-gen/gateway/v1/gateway.pb.go
#   services/proto-gen/gateway/v1/gateway_grpc.pb.go
#   services/proto-gen/risk/v1/risk.pb.go
#   services/proto-gen/risk/v1/risk_grpc.pb.go
#   services/proto-gen/transactions/v1/...
```

Each service's `go.mod` has this replace directive:

```
replace github.com/sentinelswitch/proto => ../proto-gen
```

This means each service resolves `github.com/sentinelswitch/proto` to the local `services/proto-gen/` directory. Run `buf generate` once before building any service, and again whenever `.proto` files change.

Verify generation succeeded:

```bash
ls services/proto-gen/gateway/v1/
# gateway.pb.go  gateway_grpc.pb.go
```

---

## 5. Step 2 — Risk Service

Risk Service must start **before** Fraud Engine because Fraud Engine opens a gRPC connection pool to Risk Service at startup.

### 5.1 Local Run (without Docker)

```bash
cd services/risk-service

# Download dependencies
go mod download

# Required environment variables (none are mandatory for local dev with defaults)
export LOG_LEVEL=info
export DEPLOY_ENV=local
# CONFIG_PATH defaults to config/risk-service.yaml

# Run from the service directory so config/ is resolvable
go run ./cmd/main.go
```

Expected startup output:

```
{"level":"info","msg":"risk-service gRPC server starting","addr":"[::]:50052"}
{"level":"info","msg":"metrics server listening","port":9094}
{"level":"info","msg":"health server listening","port":8084}
```

### 5.2 Ports

| Protocol | Port | Purpose |
|---|---|---|
| gRPC | 50052 | `RiskService.CalculateRisk` — called by Fraud Engine |
| HTTP | 9094 | Prometheus `/metrics` |
| HTTP | 8084 | Health check `/healthz` and `/readyz` |

### 5.3 Verify

```bash
# Health check
curl -s http://localhost:8084/health
# ok

# Metrics
curl -s http://localhost:9094/metrics | grep sentinel_risk

# gRPC reflection (requires grpcurl)
grpcurl -plaintext localhost:50052 list
# risk.v1.RiskService
```

### 5.4 Build Binary

```bash
cd services/risk-service
go build -o bin/risk-service ./cmd/main.go
./bin/risk-service
```

### 5.5 Docker Build

```bash
# Build from services/risk-service/ directory (context includes config/)
docker build \
  --build-arg VERSION=1.0.0 \
  -t sentinelswitch/risk-service:latest \
  services/risk-service/

docker run -d \
  --name sentinel-risk-service \
  -p 50052:50052 \
  -p 9094:9094 \
  -p 8084:8084 \
  -e LOG_LEVEL=info \
  sentinelswitch/risk-service:latest
```

---

## 6. Step 3 — Fraud Engine

Fraud Engine depends on: Kafka (topics must exist), Redis (DBs 0 and 2), Risk Service (gRPC on port 50052), and PostgreSQL (merchants table for warm-up).

### 6.1 Local Run (without Docker)

```bash
cd services/fraud-engine

go mod download

# Set required environment variables
export KAFKA_BROKERS=localhost:9092
export SCHEMA_REGISTRY_URL=http://localhost:8081
export RISK_SERVICE_HOST=localhost
export RISK_SERVICE_PORT=50052
export LOG_LEVEL=info
export DEPLOY_ENV=local
# Redis defaults to localhost:6379 (configured in config/fraud-engine.yaml)
# CONFIG_PATH defaults to config/fraud-engine.yaml

go run ./cmd/main.go
```

Expected startup output:

```
{"level":"info","msg":"config loaded"}
{"level":"info","msg":"merchant risk warm-up complete","loaded":1234}
{"level":"info","msg":"kafka consumer started","topic":"transactions","group":"fraud-engine-cg"}
{"level":"info","msg":"risk service connection pool ready","host":"localhost","port":50052}
```

### 6.2 Ports

| Protocol | Port | Purpose |
|---|---|---|
| HTTP | 9095 | Prometheus `/metrics` |
| HTTP | 8082 | Health check `/healthz` and `/readyz` |

Fraud Engine has **no inbound gRPC** — it only calls out to Risk Service.

### 6.3 Rules Hot Reload

Fraud Engine watches `config/fraud-rules.yaml` for changes every 30 seconds. To test a rule change:

```bash
# Edit the rules file while the service is running
nano config/fraud-rules.yaml

# Within 30 s, you should see in the logs:
# {"level":"info","msg":"rules reloaded"}
```

No restart is required. The hot-reload is atomic — the new rules are loaded and validated before replacing the old ones. If the new file is invalid, the old rules stay active and a warning is logged.

### 6.4 Verify

```bash
# Health check
curl -s http://localhost:8082/healthz
# {"status":"ok"}

# Metrics
curl -s http://localhost:9095/metrics | grep sentinel_
```

### 6.5 Circuit Breaker State

Check the current state of the Risk Service circuit breaker:

```bash
redis-cli -n 2 get cb:risk_service:state
# (nil) = CLOSED (default)
# "OPEN"
# "HALF_OPEN"

redis-cli -n 2 get cb:risk_service:failures
# "0"
```

### 6.6 Build Binary

```bash
cd services/fraud-engine
go build -o bin/fraud-engine ./cmd/main.go
./bin/fraud-engine
```

### 6.7 Docker Build

```bash
docker build \
  -t sentinelswitch/fraud-engine:latest \
  services/fraud-engine/

docker run -d \
  --name sentinel-fraud-engine \
  -p 9095:9095 \
  -p 8082:8082 \
  -e KAFKA_BROKERS=localhost:9092 \
  -e SCHEMA_REGISTRY_URL=http://localhost:8081 \
  -e RISK_SERVICE_HOST=host.docker.internal \
  -e RISK_SERVICE_PORT=50052 \
  -e LOG_LEVEL=info \
  sentinelswitch/fraud-engine:latest
```

---

## 7. Step 4 — API Gateway

API Gateway exposes the gRPC endpoint that external clients call with `SubmitTransaction`. It depends on: Redis (DB 1 for idempotency), Kafka (to publish transactions). It does NOT depend on Risk Service or PostgreSQL.

### 7.1 Local Run (without Docker)

```bash
cd services/api-gateway

go mod download

# Required — service refuses to start if unset
export PAN_HASH_SECRET="your-local-dev-secret-32-chars-min"

# Optional overrides
export KAFKA_BROKERS=localhost:9092
export SCHEMA_REGISTRY_URL=http://localhost:8081
export LOG_LEVEL=info
export DEPLOY_ENV=local
# Redis defaults to localhost:6379 (for idempotency DB 1)
# CONFIG_PATH defaults to config/api-gateway.yaml

go run ./cmd/main.go
```

Expected startup output:

```
{"level":"info","msg":"config loaded","path":"config/api-gateway.yaml"}
{"level":"info","msg":"redis connected","addr":"localhost:6379"}
{"level":"info","msg":"gRPC server starting","port":50051}
{"level":"info","msg":"HTTP server starting","port":9091}
```

> **Critical:** `PAN_HASH_SECRET` is mandatory. The service calls `os.Exit(1)` if the variable is unset. Use a random 32+ character string for local dev. In production, inject this from a secrets manager.

### 7.2 Ports

| Protocol | Port | Purpose |
|---|---|---|
| gRPC | 50051 | `GatewayService.SubmitTransaction` (client-facing) |
| HTTP | 9091 | Prometheus `/metrics` |
| HTTP | 8081 | Health check `/healthz` and `/readyz` |

### 7.3 Verify

```bash
# Health check
curl -s http://localhost:8081/healthz
# HTTP 200

# gRPC reflection (requires grpcurl)
grpcurl -plaintext localhost:50051 list
# gateway.v1.GatewayService

grpcurl -plaintext localhost:50051 describe gateway.v1.GatewayService
```

### 7.4 Redis Idempotency Check

After submitting a transaction:

```bash
redis-cli -n 1 keys 'idempotency:*'
# "idempotency:550e8400-e29b-41d4-a716-446655440000"

redis-cli -n 1 ttl 'idempotency:550e8400-e29b-41d4-a716-446655440000'
# 86399 (approximately 24 hours remaining)
```

### 7.5 Build Binary

```bash
cd services/api-gateway
go build -o bin/api-gateway ./cmd/main.go
PAN_HASH_SECRET="your-local-dev-secret" ./bin/api-gateway
```

### 7.6 Docker Build

```bash
docker build \
  --build-arg VERSION=1.0.0 \
  -t sentinelswitch/api-gateway:latest \
  services/api-gateway/

docker run -d \
  --name sentinel-api-gateway \
  -p 50051:50051 \
  -p 9091:9091 \
  -p 8081:8081 \
  -e PAN_HASH_SECRET="your-local-dev-secret-32-chars-min" \
  -e KAFKA_BROKERS=localhost:9092 \
  -e SCHEMA_REGISTRY_URL=http://localhost:8081 \
  -e LOG_LEVEL=info \
  sentinelswitch/api-gateway:latest
```

---

## 8. Step 5 — Persistence Service

Persistence Service consumes `fraud_results` from Kafka and upserts into PostgreSQL. It depends on: Kafka (topics must exist), PostgreSQL (`transactions` table must exist from migration 001).

### 8.1 Local Run (without Docker)

```bash
cd services/persistence-svc

go mod download

# Required — service refuses to start if unset
export POSTGRES_PASSWORD=sentinel_local_secret

# Optional overrides
export KAFKA_BROKERS=localhost:9092
export SCHEMA_REGISTRY_URL=http://localhost:8081
export POSTGRES_HOST=localhost
export POSTGRES_DB=sentinelswitch
export POSTGRES_USER=sentinel
export LOG_LEVEL=info
export DEPLOY_ENV=local
# CONFIG_PATH defaults to config/persistence-svc.yaml

go run ./cmd/main.go
```

Expected startup output:

```
{"level":"info","msg":"config loaded"}
{"level":"info","msg":"postgres connected","host":"localhost","db":"sentinelswitch"}
{"level":"info","msg":"kafka consumer started","topic":"fraud_results","group":"persistence-svc-cg"}
```

### 8.2 Ports

| Protocol | Port | Purpose |
|---|---|---|
| HTTP | 9093 | Prometheus `/metrics` |
| HTTP | 8083 | Health check `/healthz` and `/readyz` |

Persistence Service has no inbound gRPC — it only reads from Kafka and writes to PostgreSQL.

### 8.3 Verify

```bash
# Health check
curl -s http://localhost:8083/healthz
# HTTP 200

# Metrics
curl -s http://localhost:9093/metrics | grep sentinel_persistence
```

After transactions flow through the full pipeline:

```bash
psql -h localhost -U sentinel -d sentinelswitch \
  -c "SELECT txn_id, status, risk_score, decision FROM transactions ORDER BY created_at DESC LIMIT 5;"
```

### 8.4 DLQ Monitoring

If PostgreSQL is unavailable, after 3 retry attempts messages land in `transaction_dlq`:

```bash
# Check DLQ offset lag
kafka-consumer-groups --bootstrap-server localhost:9092 \
  --group dlq-retry-worker-cg --describe

# Consume DLQ messages to inspect failure headers
kafka-console-consumer \
  --bootstrap-server localhost:9092 \
  --topic transaction_dlq \
  --from-beginning \
  --property print.headers=true
```

### 8.5 Build Binary

```bash
cd services/persistence-svc
go build -o bin/persistence-svc ./cmd/main.go
POSTGRES_PASSWORD=sentinel_local_secret ./bin/persistence-svc
```

### 8.6 Docker Build

```bash
docker build \
  -t sentinelswitch/persistence-svc:latest \
  services/persistence-svc/

docker run -d \
  --name sentinel-persistence-svc \
  -p 9093:9093 \
  -p 8083:8083 \
  -e POSTGRES_PASSWORD=sentinel_local_secret \
  -e POSTGRES_HOST=host.docker.internal \
  -e KAFKA_BROKERS=localhost:9092 \
  -e SCHEMA_REGISTRY_URL=http://localhost:8081 \
  -e LOG_LEVEL=info \
  sentinelswitch/persistence-svc:latest
```

---

## 9. Running All Services with Docker Compose

Add the four service definitions to the `docker-compose.yml` from [INFRASTRUCTURE_SETUP.md](INFRASTRUCTURE_SETUP.md):

```yaml
  # ---------------------------------------------------------------------------
  # Risk Service — must start before Fraud Engine
  # ---------------------------------------------------------------------------
  risk-service:
    build:
      context: services/risk-service
    image: sentinelswitch/risk-service:latest
    container_name: sentinel-risk-service
    depends_on:
      kafka:
        condition: service_healthy
    ports:
      - "50052:50052"
      - "9094:9094"
      - "8084:8084"
    environment:
      LOG_LEVEL: info
      DEPLOY_ENV: local
      CONFIG_PATH: /config/risk-service.yaml
    volumes:
      - ./config/risk-service.yaml:/config/risk-service.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8084/health"]
      interval: 10s
      timeout: 5s
      retries: 10

  # ---------------------------------------------------------------------------
  # Fraud Engine
  # ---------------------------------------------------------------------------
  fraud-engine:
    build:
      context: services/fraud-engine
    image: sentinelswitch/fraud-engine:latest
    container_name: sentinel-fraud-engine
    depends_on:
      kafka:
        condition: service_healthy
      redis:
        condition: service_healthy
      risk-service:
        condition: service_healthy
    ports:
      - "9095:9095"
      - "8082:8082"
    environment:
      KAFKA_BROKERS: kafka:9093
      SCHEMA_REGISTRY_URL: http://schema-registry:8081
      RISK_SERVICE_HOST: risk-service
      RISK_SERVICE_PORT: "50052"
      LOG_LEVEL: info
      DEPLOY_ENV: local
    volumes:
      - ./config/fraud-engine.yaml:/config/fraud-engine.yaml:ro
      - ./config/fraud-rules.yaml:/config/fraud-rules.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8082/healthz"]
      interval: 10s
      timeout: 5s
      retries: 10

  # ---------------------------------------------------------------------------
  # API Gateway
  # ---------------------------------------------------------------------------
  api-gateway:
    build:
      context: services/api-gateway
    image: sentinelswitch/api-gateway:latest
    container_name: sentinel-api-gateway
    depends_on:
      kafka:
        condition: service_healthy
      redis:
        condition: service_healthy
    ports:
      - "50051:50051"
      - "9091:9091"
      - "8081:8081"
    environment:
      KAFKA_BROKERS: kafka:9093
      SCHEMA_REGISTRY_URL: http://schema-registry:8081
      PAN_HASH_SECRET: "${PAN_HASH_SECRET}"    # injected from .env file
      LOG_LEVEL: info
      DEPLOY_ENV: local
    volumes:
      - ./config/api-gateway.yaml:/config/api-gateway.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8081/healthz"]
      interval: 10s
      timeout: 5s
      retries: 10

  # ---------------------------------------------------------------------------
  # Persistence Service
  # ---------------------------------------------------------------------------
  persistence-svc:
    build:
      context: services/persistence-svc
    image: sentinelswitch/persistence-svc:latest
    container_name: sentinel-persistence-svc
    depends_on:
      kafka:
        condition: service_healthy
      postgres:
        condition: service_healthy
    ports:
      - "9093:9093"
      - "8083:8083"
    environment:
      KAFKA_BROKERS: kafka:9093
      SCHEMA_REGISTRY_URL: http://schema-registry:8081
      POSTGRES_HOST: postgres
      POSTGRES_DB: sentinelswitch
      POSTGRES_USER: sentinel
      POSTGRES_PASSWORD: "${POSTGRES_PASSWORD}"   # injected from .env file
      LOG_LEVEL: info
      DEPLOY_ENV: local
    volumes:
      - ./config/persistence-svc.yaml:/config/persistence-svc.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8083/healthz"]
      interval: 10s
      timeout: 5s
      retries: 10
```

Create a `.env` file at the repository root (never commit this):

```bash
# .env
PAN_HASH_SECRET=your-local-dev-secret-at-least-32-chars
POSTGRES_PASSWORD=sentinel_local_secret
```

Start everything:

```bash
# Build all service images
docker compose build

# Start infrastructure first
docker compose up -d zookeeper kafka schema-registry redis postgres

# Wait for infrastructure to be healthy
docker compose ps

# Create Kafka topics
BROKER=localhost:9092
kafka-topics --bootstrap-server $BROKER --create --topic transactions --partitions 12 --replication-factor 1 --config retention.ms=259200000
kafka-topics --bootstrap-server $BROKER --create --topic fraud_results --partitions 12 --replication-factor 1 --config retention.ms=604800000
kafka-topics --bootstrap-server $BROKER --create --topic transaction_dlq --partitions 3 --replication-factor 1 --config retention.ms=1209600000

# Generate proto code
buf generate

# Start services in dependency order
docker compose up -d risk-service
docker compose up -d fraud-engine persistence-svc api-gateway

# Verify all healthy
docker compose ps
```

---

## 10. Startup Order and Dependencies

```
Infrastructure:
  Zookeeper → Kafka → Schema Registry
  Redis (independent)
  PostgreSQL (independent)

Services:
  Risk Service               (depends on: nothing from SentinelSwitch)
       ↓
  Fraud Engine               (depends on: Kafka, Redis DB 0+2, Risk Service)
  Persistence Service        (depends on: Kafka, PostgreSQL)
  API Gateway                (depends on: Kafka, Redis DB 1)
```

**Risk Service must be healthy before Fraud Engine starts.** Fraud Engine opens a gRPC connection pool to Risk Service at init time. If Risk Service is unavailable at startup, the circuit breaker opens immediately and fallback scores (600 / REVIEW) are used for all transactions until Risk Service recovers.

**API Gateway and Persistence Service can start in parallel** — they do not depend on each other or on Risk Service.

---

## 11. Environment File Reference

Use this as your `.env` template for local development:

```bash
# ============================================================
# SentinelSwitch — Local Development Environment Variables
# DO NOT commit this file. Add to .gitignore.
# ============================================================

# --- Shared ---
KAFKA_BROKERS=localhost:9092
SCHEMA_REGISTRY_URL=http://localhost:8081
LOG_LEVEL=info
DEPLOY_ENV=local
SERVICE_VERSION=dev

# --- API Gateway ---
PAN_HASH_SECRET=local-dev-secret-replace-with-32-plus-chars
GATEWAY_GRPC_PORT=50051
GATEWAY_METRICS_PORT=9091
GATEWAY_HEALTH_PORT=8081
RATE_LIMITING_ENABLED=true

# --- Fraud Engine ---
RISK_SERVICE_HOST=localhost
RISK_SERVICE_PORT=50052
FRAUD_ENGINE_WORKERS=10
FRAUD_ENGINE_METRICS_PORT=9095
FRAUD_ENGINE_HEALTH_PORT=8082
RULES_CONFIG_FILE=config/fraud-rules.yaml

# --- Risk Service ---
RISK_SERVICE_GRPC_PORT=50052
RISK_SERVICE_METRICS_PORT=9094
RISK_SERVICE_HEALTH_PORT=8084
RISK_SERVICE_MAX_STREAMS=500
RISK_SERVICE_WORKERS=0

# --- Persistence Service ---
POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DB=sentinelswitch
POSTGRES_USER=sentinel
POSTGRES_PASSWORD=sentinel_local_secret
POSTGRES_MAX_OPEN=20
POSTGRES_MAX_IDLE=5
PERSISTENCE_SVC_METRICS_PORT=9093
PERSISTENCE_SVC_HEALTH_PORT=8083
```

---

## 12. Sending a Test Transaction

### 12.1 via grpcurl

```bash
# Submit a transaction
grpcurl -plaintext \
  -d '{
    "txn_id": "550e8400-e29b-41d4-a716-446655440000",
    "card_number": "4111111111111111",
    "amount_minor": 50000,
    "currency": "OMR",
    "merchant_id": "M00000000000123",
    "transaction_type": "sales",
    "channel": "pos",
    "scheme": "VISA",
    "rrn": "123456789012",
    "terminal_id": "T0000456",
    "mcc": "5411",
    "txn_timestamp": "2025-06-15T14:30:00Z"
  }' \
  localhost:50051 \
  gateway.v1.GatewayService/SubmitTransaction

# Expected response (ACK — non-blocking):
# {
#   "txn_id": "550e8400-e29b-41d4-a716-446655440000",
#   "status": "RECEIVED"
# }
```

### 12.2 Trace a Transaction Through the Pipeline

After submitting:

1. **API Gateway** — validates, hashes PAN, writes idempotency key to Redis DB 1, publishes `TransactionEvent` to Kafka `transactions` topic.

2. **Fraud Engine** — consumes from `transactions`, runs:
   - Rule engine (fast checks: amount, MCC, velocity rules)
   - Velocity counters (Redis DB 0 sorted sets)
   - gRPC `CalculateRisk` call to Risk Service
   - Merges triggered rules + risk score → Decision
   - Publishes `FraudResultEvent` to Kafka `fraud_results` topic

3. **Risk Service** — receives feature vector, returns `risk_score` (0–1000) and `decision` recommendation.

4. **Persistence Service** — consumes from `fraud_results`, upserts into PostgreSQL `transactions` table.

### 12.3 Check the Result

```bash
# Check PostgreSQL — result should appear within ~1 second
psql -h localhost -U sentinel -d sentinelswitch \
  -c "SELECT txn_id, status, risk_score, triggered_rules, processed_at
      FROM transactions
      WHERE txn_id = '550e8400-e29b-41d4-a716-446655440000';"

# Decision thresholds:
# risk_score < 400  → status = APPROVED
# 400–700           → status = REVIEW
# > 700             → status = DECLINED
```

### 12.4 Test Velocity Trigger

Send 5 transactions with the same card within 60 seconds to trigger `VELOCITY_SPIKE`:

```bash
for i in $(seq 1 6); do
  grpcurl -plaintext \
    -d "{
      \"card_number\": \"4111111111111111\",
      \"amount_minor\": 10000,
      \"currency\": \"OMR\",
      \"merchant_id\": \"M00000000000123\",
      \"transaction_type\": \"sales\",
      \"channel\": \"pos\",
      \"mcc\": \"5411\",
      \"rrn\": \"12345678900${i}\",
      \"terminal_id\": \"T0000001\"
    }" \
    localhost:50051 gateway.v1.GatewayService/SubmitTransaction
done
```

Expect later transactions to have elevated `risk_score` and `triggered_rules` like `["VELOCITY_SPIKE"]`.

---

## 13. Observing the Pipeline

### 13.1 Logs

Each service emits structured JSON logs. To follow logs for a service:

```bash
# Docker Compose
docker compose logs -f fraud-engine
docker compose logs -f api-gateway

# Local binary
# Logs stream directly to stdout — pipe through jq for readability:
go run ./cmd/main.go | jq .
```

A fraud decision log line from Fraud Engine looks like:

```json
{
  "level": "info",
  "msg": "fraud decision",
  "txn_id": "550e8400-e29b-41d4-a716-446655440000",
  "card_hash": "aabbcc...",
  "risk_score": 350,
  "decision": "APPROVE",
  "triggered_rules": [],
  "latency_ms": 12
}
```

### 13.2 Kafka Consumer Lag

```bash
BROKER=localhost:9092

# Fraud Engine consumer lag
kafka-consumer-groups --bootstrap-server $BROKER \
  --group fraud-engine-cg --describe

# Persistence Service consumer lag
kafka-consumer-groups --bootstrap-server $BROKER \
  --group persistence-svc-cg --describe
```

Healthy state: LAG column should be 0 or near 0 under normal load.

### 13.3 Redis State

```bash
# Velocity sorted sets (DB 0)
redis-cli -n 0 keys 'velocity:count:*' | head -20
redis-cli -n 0 zrangebyscore velocity:count:TXN_COUNT_1MIN:<card_hash> -inf +inf WITHSCORES

# Idempotency keys (DB 1)
redis-cli -n 1 dbsize

# Circuit breaker state (DB 2)
redis-cli -n 2 get cb:risk_service:state
redis-cli -n 2 get cb:risk_service:failures
```

### 13.4 Prometheus Metrics

```bash
# Check all service metrics are being scraped
curl -s http://localhost:9090/api/v1/targets | jq '.data.activeTargets[] | {job: .labels.job, health: .health}'

# Query risk score distribution
curl -s "http://localhost:9090/api/v1/query?query=sentinel_risk_score_histogram_bucket" | jq .

# Query DLQ publish rate
curl -s "http://localhost:9090/api/v1/query?query=rate(sentinel_persistence_dlq_publishes_total[5m])" | jq .
```

---

## 14. Graceful Shutdown

All four services handle `SIGINT` and `SIGTERM` with a graceful shutdown sequence:

**API Gateway:** stops accepting new gRPC calls → drains in-flight requests → closes Kafka producer → closes Redis connection.

**Fraud Engine:** stops consuming from Kafka → waits for in-flight pipeline to complete → closes gRPC connection pool → closes Redis connections.

**Risk Service:** stops accepting new gRPC calls → waits for in-flight scoring to complete → shuts down metrics and health HTTP servers.

**Persistence Service:** stops consuming from Kafka → flushes the current DB batch → commits all pending Kafka offsets → closes PostgreSQL connection pool.

Shutdown timeout: **30 seconds** for each service. If the timeout expires, the process exits with code 1.

To gracefully stop a service:

```bash
# Docker Compose (sends SIGTERM, waits up to 30 s)
docker compose stop fraud-engine

# Local binary
# Press Ctrl+C (sends SIGINT)
# Or: kill -TERM <pid>
```

---

## 15. Common Errors and Fixes

### `PAN_HASH_SECRET: required environment variable not set`

**Cause:** API Gateway requires `PAN_HASH_SECRET` at startup.
**Fix:** `export PAN_HASH_SECRET="your-secret-here"` before running.

---

### `POSTGRES_PASSWORD: required environment variable not set`

**Cause:** Persistence Service requires `POSTGRES_PASSWORD` at startup.
**Fix:** `export POSTGRES_PASSWORD=sentinel_local_secret` before running.

---

### `failed to connect to Kafka: dial tcp localhost:9092: connection refused`

**Cause:** Kafka is not running or not yet healthy.
**Fix:**
```bash
docker compose up -d kafka
docker compose ps kafka    # wait for "healthy"
```

---

### `topic "transactions" does not exist`

**Cause:** Topics must be created manually (`auto.create.topics.enable=false`).
**Fix:**
```bash
kafka-topics --bootstrap-server localhost:9092 --create --topic transactions --partitions 12 --replication-factor 1
kafka-topics --bootstrap-server localhost:9092 --create --topic fraud_results --partitions 12 --replication-factor 1
kafka-topics --bootstrap-server localhost:9092 --create --topic transaction_dlq --partitions 3 --replication-factor 1
```

---

### `failed to dial risk-service:50052: connection refused`

**Cause:** Fraud Engine cannot reach Risk Service. Risk Service must start before Fraud Engine.
**Fix:** Start Risk Service first and wait for its health check to pass:
```bash
docker compose up -d risk-service
curl -s http://localhost:8084/health   # must return "ok" before starting Fraud Engine
docker compose up -d fraud-engine
```

---

### `could not find module providing package github.com/sentinelswitch/proto`

**Cause:** `buf generate` has not been run yet — `services/proto-gen/` is empty.
**Fix:**
```bash
buf generate
# Then retry go mod download and go build
```

---

### `relation "transactions" does not exist`

**Cause:** PostgreSQL migration has not been applied.
**Fix:**
```bash
psql -h localhost -U sentinel -d sentinelswitch \
  -f db/migrations/001_create_transactions.sql
```

If using Docker Compose, ensure the migration file is mounted into `/docker-entrypoint-initdb.d/` as shown in [INFRASTRUCTURE_SETUP.md](INFRASTRUCTURE_SETUP.md). Note this only runs on first container creation — if the volume already exists, delete it first:
```bash
docker compose down -v
docker compose up -d postgres
```

---

### `redis: connection refused` at API Gateway startup

**Cause:** Redis is not running.
**Fix:**
```bash
docker compose up -d redis
```
API Gateway continues with `on_redis_unavailable: allow_through` — transactions will flow but idempotency is not enforced until Redis recovers. A `sentinel_idempotency_redis_miss_total` counter increments for each affected request.

---

### Circuit breaker stuck OPEN after Risk Service restarts

**Cause:** The `cb:risk_service:state` key in Redis DB 2 may outlive the `state_ttl_seconds: 60` if Redis itself was restarted without eviction.
**Fix:**
```bash
redis-cli -n 2 del cb:risk_service:state cb:risk_service:failures
```
The circuit breaker will reset to CLOSED on the next Fraud Engine evaluation cycle.
