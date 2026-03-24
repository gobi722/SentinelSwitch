# 🚀 SentinelSwitch

SentinelSwitch is a distributed, event-driven payment transaction and fraud monitoring platform built using Go.  
It simulates a real-world payment switch architecture using Kafka, gRPC, PostgreSQL, Redis, Prometheus, Grafana, Docker, and Kubernetes.

This project demonstrates scalable microservice architecture, real-time fraud scoring, async processing, and production-grade observability.

---

## 🧠 Architecture Overview

                      ┌────────────────────┐
                │    API Gateway     │  (Go Fiber)
                └─────────┬──────────┘
                          │
                          ▼
                ┌────────────────────┐
                │   Kafka Producer   │
                │  (transaction-topic)
                └─────────┬──────────┘
                          │
        ┌─────────────────┼─────────────────┐
        ▼                                   ▼
┌────────────────────┐           ┌────────────────────────┐
│   Fraud Engine     │           │   Persistence Service   │
│   (Kafka Consumer) │           │   (Kafka Consumer)      │
└─────────┬──────────┘           └──────────┬──────────────┘
          │                                 │
          ▼                                 ▼
┌────────────────────┐           ┌────────────────────────┐
│ Risk Scoring Svc   │           │      PostgreSQL        │
│     (gRPC)         │           │ (Partitioned Storage)  │
└─────────┬──────────┘           └────────────────────────┘
          │
          ▼
┌────────────────────┐
│ Kafka Producer     │
│ (fraud-result-topic)
└────────────────────┘

All services expose Prometheus metrics → scraped by Prometheus → visualized in Grafana


---

## 🧩 Tech Stack

| Component        | Technology Used |
|------------------|-----------------|
| Language         | Go (Golang)     |
| API Framework    | Fiber           |
| Messaging        | Apache Kafka    |
| RPC              | gRPC            |
| Database         | PostgreSQL      |
| Cache            | Redis           |
| Metrics          | Prometheus      |
| Visualization    | Grafana         |
| Containerization | Docker          |
| Orchestration    | Kubernetes      |

---

## 🔥 Microservices

### 1️⃣ API Gateway
- Accepts transaction requests
- Publishes to Kafka
- Returns immediate acknowledgement
- Exposes Prometheus metrics

### 2️⃣ Fraud Engine Service
- Consumes transactions from Kafka
- Performs rule-based + velocity fraud checks
- Calls Risk Scoring service via gRPC
- Publishes fraud result to Kafka

### 3️⃣ Risk Scoring Service
- gRPC-based microservice
- Calculates risk score (100–1000)
- Simulates ML model scoring

### 4️⃣ Persistence Service
- Consumes transaction + fraud results
- Stores into PostgreSQL
- Maintains partitioned transaction tables

---

## 📊 Observability

Each service exports:

- transactions_total
- fraud_detected_total
- grpc_request_duration_seconds
- kafka_consumer_lag
- db_write_latency_seconds
- api_request_count

Prometheus scrapes metrics.
Grafana dashboards visualize:

- TPS (Transactions per second)
- Fraud detection ratio
- gRPC latency
- Consumer lag
- DB latency

---

## 🐳 Running with Docker Compose

### 1️⃣ Clone the repository

```bash
git clone https://github.com/your-username/sentinelswitch.git
cd sentinelswitch
