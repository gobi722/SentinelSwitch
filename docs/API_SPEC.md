# SentinelSwitch — API Specification

**Transport:** gRPC / Protocol Buffers (proto3)
**Source of truth:** [`proto/gateway.proto`](../proto/gateway.proto) — this document is a human-readable
companion to it, not a replacement. If the two ever disagree, the `.proto` file wins; please flag the
drift.
**Host:** API Gateway — gRPC port `50051` (see [docs/INFRASTRUCTURE_SETUP.md](INFRASTRUCTURE_SETUP.md)
for ports/env vars, [README.md](../README.md) for `docker compose up -d` / local run instructions).

This surface has two unrelated services, gated by two different secrets:

| Service | Who calls it | Auth header | Purpose |
|---|---|---|---|
| `GatewayService` | External integrators (POS terminals, payment-page SDKs) | `x-api-key` (per-client) | Submit transactions, poll fraud decisions |
| `AdminService` | Operators only | `x-admin-key` (single operator secret) | Onboard new clients |

---

## Table of Contents

1. [Authentication](#1-authentication)
2. [GatewayService.SubmitTransaction](#2-gatewayservicesubmittransaction)
3. [GatewayService.GetTransactionStatus](#3-gatewayservicegettransactionstatus)
4. [AdminService.ProvisionClient](#4-adminserviceprovisionclient)
5. [Enums](#5-enums)
6. [Error codes — full reference](#6-error-codes--full-reference)
7. [Rate limiting](#7-rate-limiting)
8. [Known gaps in this spec's own config](#8-known-gaps-in-this-specs-own-config)

---

## 1. Authentication

### `GatewayService` — `x-api-key`

Every `GatewayService` RPC (both of them) must carry a gRPC metadata header:

```
x-api-key: <client's api key>
```

Enforced by `auth.UnaryServerInterceptor` (`services/api-gateway/internal/auth/interceptor.go`) for
every method **except** `/grpc.health.v1.Health/*` and `/sentinel.gateway.v1.AdminService/*`. The key
is SHA-256-hashed and looked up against the `api_clients` table (Postgres, Redis-cached, 60 s TTL).
The verified `client_id` this resolves to is injected into the request context and is what
`merchant_id` (client-supplied, "who the transaction is *for*") is deliberately kept separate from.

**Fails closed**: a Postgres or Redis outage during lookup returns `UNAVAILABLE` — it never falls back
to admitting an unauthenticated request.

Get an API key via [§4 AdminService.ProvisionClient](#4-adminserviceprovisionclient).

### `AdminService` — `x-admin-key`

```
x-admin-key: <ADMIN_API_KEY env var value>
```

A single, higher-privilege, operator-only secret — never issued to external callers, unrelated to any
client's `x-api-key`. Checked directly in `gateway.AdminHandler.checkAdminKey`
(`services/api-gateway/internal/gateway/admin_handler.go`) with `hmac.Equal` (constant-time). The
service refuses to start if `ADMIN_API_KEY` is unset (same fail-closed startup pattern as
`PAN_HASH_SECRET` / `POSTGRES_PASSWORD`).

---

## 2. `GatewayService.SubmitTransaction`

Submits a transaction for asynchronous fraud analysis. Returns immediately (does not wait for the
fraud decision). SLA: P99 < 50 ms for the gateway hop itself.

```
rpc SubmitTransaction (TransactionRequest) returns (TransactionAck);
```

### Request — `TransactionRequest`

| Field | Type | Required | Validation (as enforced by `internal/gateway/validator.go`) |
|---|---|---|---|
| `pan_last4` | string | Yes | Exactly 4 digits |
| `card_expiry` | string | No | `MM/YY` — not currently validated by the server (accepted as-is) |
| `card_hash` | string | Yes | Non-empty. Set by the client SDK/terminal; the gateway re-applies its own `HMAC-SHA256(SHA-256(x), PAN_HASH_SECRET)` to whatever arrives here to derive the internal `card_hash` used for Kafka partitioning and Redis velocity keys |
| `amount_minor` | int64 | Yes | Between `validation.amount_min_minor` and `amount_max_minor` (`config/api-gateway.yaml`; default 1 – 999,999,999,999) |
| `currency` | string | Yes | Case-insensitive match against `validation.supported_currencies` (default: `OMR, INR, USD, AED, SAR, BHD, KWD, QAR`) |
| `merchant_id` | string | No | ≤ `validation.merchant_id_max_length` (default 20) chars. Business data — which merchant the txn is *for*, distinct from the caller's own `client_id` |
| `mcc` | string | No | If present, must match `validation.mcc_pattern` (default `^[0-9]{4}$`) |
| `transaction_type` | `TransactionType` enum | Yes | Must not be `TRANSACTION_TYPE_UNSPECIFIED` (i.e. must be one of `SALES`, `VOID`, `REFUND`, `REVERSAL` — see [§5](#5-enums)) |
| `channel` | `Channel` enum | Yes | Must not be `CHANNEL_UNSPECIFIED` (i.e. `POS` or `PG` — see [§5](#5-enums)) |
| `terminal_id` | string | No | ≤ `validation.terminal_id_max_length` (default 16) chars |
| `idempotency_key` | string | Yes | Non-empty after trimming. Recommended: client-generated UUID v4. A duplicate key within the 24 h window short-circuits to the original `TransactionAck` — see below |

`client_id` is **not** a request field — it is derived server-side from the verified `x-api-key` and
never accepted from the caller.

### Response — `TransactionAck`

| Field | Type | Notes |
|---|---|---|
| `txn_id` | string | Server-assigned UUID v4. Use this in `GetTransactionStatus` |
| `status` | `TransactionStatus` enum | Always `PENDING` on a fresh submission |
| `submitted_at` | string | ISO 8601 UTC, e.g. `2025-08-15T09:31:00.123Z` |

### Behavior notes

- **Non-blocking**: the fraud decision is computed asynchronously by Fraud Engine → Risk Service; this
  call never waits for it. Poll [`GetTransactionStatus`](#3-gatewayservicegettransactionstatus) or
  consume the client's own `results.<client_id>` Kafka topic (see
  [docs/MULTI_TENANT_RESULT_DELIVERY.md](MULTI_TENANT_RESULT_DELIVERY.md)) for the result.
- **Idempotency**: duplicate `idempotency_key` within 24 h returns the *original* `txn_id` with gRPC
  status `ALREADY_EXISTS` (not a plain 200 — check the status code, not just the body).
- **Rate limiting**: see [§7](#7-rate-limiting).

### Example (`grpcurl`)

```bash
grpcurl -plaintext \
  -H "x-api-key: <your api key>" \
  -d '{
    "pan_last4": "1111",
    "card_expiry": "12/27",
    "card_hash": "aabbccdd...",
    "amount_minor": 50000,
    "currency": "OMR",
    "merchant_id": "M00000000000123",
    "mcc": "5411",
    "transaction_type": "SALES",
    "channel": "POS",
    "terminal_id": "T0000456",
    "idempotency_key": "550e8400-e29b-41d4-a716-446655440000"
  }' \
  localhost:50051 \
  sentinel.gateway.v1.GatewayService/SubmitTransaction
```

### Errors

| Code | When |
|---|---|
| `UNAUTHENTICATED` | Missing/invalid `x-api-key` |
| `INVALID_ARGUMENT` | Any field fails the table above (all failures are collected and joined into one message, not just the first) |
| `ALREADY_EXISTS` | Duplicate `idempotency_key` within 24 h — response body carries the *original* `txn_id` |
| `RESOURCE_EXHAUSTED` | Rate limit exceeded ([§7](#7-rate-limiting)) |
| `INTERNAL` | Idempotency store or Kafka publish failed unexpectedly — safe to retry |
| `UNAVAILABLE` | Auth backing store (Postgres/Redis) down — fails closed, never silently admits the request |

---

## 3. `GatewayService.GetTransactionStatus`

Polls the fraud decision for a previously submitted transaction. SLA: P99 < 30 ms.

```
rpc GetTransactionStatus (StatusRequest) returns (TransactionStatusResponse);
```

### Request — `StatusRequest`

| Field | Type | Required |
|---|---|---|
| `txn_id` | string | Yes — the UUID returned by `SubmitTransaction` |

### Response — `TransactionStatusResponse`

| Field | Type | Notes |
|---|---|---|
| `txn_id` | string | Echoes the request |
| `status` | `TransactionStatus` enum | `PENDING`, `APPROVED`, `DECLINED`, `REVIEW`, or `ERROR` |
| `fraud_decision` | string | `"APPROVE"` \| `"DECLINE"` \| `"REVIEW"` — populated only when `status` is `APPROVED`/`DECLINED`/`REVIEW`, empty otherwise |
| `risk_score` | int32 | 100–1000; `0` while `status` is `PENDING` |
| `submitted_at` | string | ISO 8601 UTC |
| `decided_at` | string | ISO 8601 UTC; empty while `status` is `PENDING` |

### ⚠️ Deliberately ambiguous response — read before integrating

This call does **not** return `NOT_FOUND`. Three distinct situations all produce the exact same
response — `status: PENDING`, no error:

1. `txn_id` is genuinely unknown to this system.
2. `txn_id` exists but Fraud Engine hasn't decided yet (normal — poll again shortly).
3. `txn_id` belongs to a **different** `client_id` than the one authenticated on this call.

This is intentional (`services/api-gateway/internal/gateway/handler.go`,
`internal/txnstatus/store.go`): distinguishing case 3 from cases 1–2 would let one tenant confirm
*that a transaction exists* for another tenant, which is itself a tenant-isolation leak. A caller
cannot tell "still processing" from "not yours" from "typo'd the ID" — if you need to detect a typo'd
`txn_id`, do it client-side by only ever polling IDs you received from your own `SubmitTransaction`
calls.

### Example (`grpcurl`)

```bash
grpcurl -plaintext \
  -H "x-api-key: <your api key>" \
  -d '{"txn_id": "550e8400-e29b-41d4-a716-446655440000"}' \
  localhost:50051 \
  sentinel.gateway.v1.GatewayService/GetTransactionStatus
```

### Errors

| Code | When |
|---|---|
| `UNAUTHENTICATED` | Missing/invalid `x-api-key` |
| `INVALID_ARGUMENT` | `txn_id` empty |
| `UNAVAILABLE` | Postgres lookup failed — retry |

(No `NOT_FOUND` — see the ambiguity note above.)

---

## 4. `AdminService.ProvisionClient`

Onboards a new external API client in one call: creates its `api_clients` Postgres row, a Kafka
SCRAM-SHA-256 credential, a dedicated `results.<client_id>` topic, and the ACLs scoping that
credential to only that topic plus a `<client_id>.`-prefixed consumer-group namespace. Automates what
`scripts/provision-client.sh` does by hand — see
[docs/MULTI_TENANT_RESULT_DELIVERY.md §H](MULTI_TENANT_RESULT_DELIVERY.md) for the full design and the
script's own step-by-step equivalent (kept as a break-glass path when API Gateway itself is down).

```
rpc ProvisionClient (ProvisionClientRequest) returns (ProvisionClientResponse);
```

Requires `x-admin-key` — see [§1](#1-authentication). This is **operator-triggered automation, not
open public self-service signup**: whoever holds the admin key can provision any client_id; there is
no approval workflow or per-caller restriction beyond that.

### Request — `ProvisionClientRequest`

| Field | Type | Required | Validation |
|---|---|---|---|
| `client_id` | string | Yes | Must match `^[a-zA-Z0-9._-]{1,200}$` — used verbatim as a Kafka topic-name component (`results.<client_id>`) and consumer-group prefix, so it must be legal for both. Must not already exist (`api_clients.client_id` is the primary key) |
| `display_name` | string | Yes | Non-empty |

### Response — `ProvisionClientResponse`

| Field | Type | Notes |
|---|---|---|
| `client_id` | string | Echoes the request |
| `api_key` | string | **Plaintext, shown exactly once.** Use as `x-api-key` on `GatewayService` calls. Only a SHA-256 hash is stored (Postgres) — this exact value is never retrievable again |
| `kafka_topic` | string | `results.<client_id>` |
| `scram_username` | string | Equals `client_id` |
| `scram_password` | string | **Plaintext, shown exactly once.** SASL/SCRAM-SHA-256 password for the Kafka `PUBLIC` listener (`localhost:9096` in dev). Only a PBKDF2-derived `SaltedPassword` is sent to the broker (never the plaintext) — this value is not stored anywhere and cannot be recovered later |
| `consumer_group_prefix` | string | `<client_id>.` — consumer group IDs the client chooses **must** start with this; the ACL is scoped to the prefix, not the literal topic's default group |

**Save `api_key` and `scram_password` immediately** — there is no "show again" endpoint. If lost, the
only recovery path today is re-provisioning (new client_id) or manually rotating both via direct
Postgres/Kafka access — no rotation RPC exists yet.

### Example (`grpcurl`)

```bash
grpcurl -plaintext \
  -H "x-admin-key: <ADMIN_API_KEY>" \
  -d '{"client_id": "my-test-client", "display_name": "My Test Integrator"}' \
  localhost:50051 \
  sentinel.gateway.v1.AdminService/ProvisionClient
```

### Errors

| Code | When |
|---|---|
| `UNAUTHENTICATED` | Missing `x-admin-key` |
| `PERMISSION_DENIED` | `x-admin-key` present but wrong |
| `INVALID_ARGUMENT` | `client_id`/`display_name` empty, or `client_id` fails the regex |
| `ALREADY_EXISTS` | `client_id` already provisioned |
| `INTERNAL` | A step after the Postgres insert failed (SCRAM credential / topic / ACL). **Not transactional across Postgres and Kafka** — if this happens, the Postgres row already exists but Kafka-side setup is incomplete. The server log names exactly which step failed and how to finish it by hand (or delete the `api_clients` row and retry with a fresh `client_id`) |

---

## 5. Enums

### `TransactionStatus`

| Value | Meaning |
|---|---|
| `TRANSACTION_STATUS_UNSPECIFIED` (0) | Never sent by the server; zero-value only |
| `PENDING` (1) | Submitted, awaiting a fraud decision (or, for `GetTransactionStatus`, see the ambiguity note in [§3](#3-gatewayservicegettransactionstatus)) |
| `APPROVED` (2) | Fraud Engine approved |
| `DECLINED` (3) | Fraud Engine declined |
| `REVIEW` (4) | Flagged for manual review |
| `ERROR` (5) | Processing error; retry-safe |

### `TransactionType`

| Value | Accepted on `SubmitTransaction` |
|---|---|
| `TRANSACTION_TYPE_UNSPECIFIED` (0) | No — rejected with `INVALID_ARGUMENT` |
| `SALES` (1) | Yes |
| `VOID` (2) | Yes |
| `REFUND` (3) | Yes |
| `REVERSAL` (4) | Yes |

### `Channel`

| Value | Accepted on `SubmitTransaction` |
|---|---|
| `CHANNEL_UNSPECIFIED` (0) | No — rejected with `INVALID_ARGUMENT` |
| `POS` (1) | Yes |
| `PG` (2) | Yes |

---

## 6. Error codes — full reference

| gRPC code | Services | Meaning |
|---|---|---|
| `INVALID_ARGUMENT` | Gateway, Admin | Malformed request — see per-RPC tables above |
| `UNAUTHENTICATED` | Gateway, Admin | Missing/invalid `x-api-key` (Gateway) or missing `x-admin-key` (Admin) |
| `PERMISSION_DENIED` | Admin | `x-admin-key` present but incorrect |
| `ALREADY_EXISTS` | Gateway (`SubmitTransaction`), Admin | Duplicate `idempotency_key` (Gateway) or `client_id` already provisioned (Admin) |
| `RESOURCE_EXHAUSTED` | Gateway (`SubmitTransaction`) | Rate limit exceeded |
| `INTERNAL` | Gateway, Admin | Unexpected server error — safe to retry with backoff |
| `UNAVAILABLE` | Gateway | Kafka, Redis, or Postgres unreachable — retry. Auth fails **closed**: never falls back to admitting an unauthenticated request |

---

## 7. Rate limiting

Per-client token bucket (`golang.org/x/time/rate`), in-memory per Gateway replica —
`services/api-gateway/internal/ratelimit/limiter.go`:

| Setting | Default (`config/api-gateway.yaml → rate_limiting`) |
|---|---|
| Requests/second | 100 |
| Burst | 200 |
| Bucket key | The verified `client_id` (the interceptor overwrites the configured identity header with the authenticated value before the rate limiter reads it — a caller cannot spoof a different bucket) |

Exceeding the bucket returns `RESOURCE_EXHAUSTED` on `SubmitTransaction`. `GetTransactionStatus` and
`ProvisionClient` are not currently rate-limited.

---

## 8. Known gaps in this spec's own config

Found while writing this document, not previously documented anywhere:

- `config/api-gateway.yaml`'s `validation.supported_transaction_types` (`sales, refund, void, auth`)
  and `validation.supported_channels` (`pos, pg, atm, moto`) are **not enforced** by
  `internal/gateway/validator.go` — it only checks the proto enum isn't the zero value. The real
  constraint is the `TransactionType`/`Channel` enums in [§5](#5-enums), which do **not** include
  `auth`, `atm`, or `moto`. Sending those as strings isn't possible anyway (the field is a typed enum,
  not a string), so this is a stale/misleading YAML list rather than a functional bug — but if you're
  reading the YAML to learn what's accepted, trust this document (and the `.proto` enum) instead.
- `validation.rrn_pattern` in the same file has no corresponding field on `TransactionRequest` at all
  (no `rrn` field exists in the current proto) — also unused by the validator.
