-- =============================================================================
-- SentinelSwitch — Migration 001
-- Table  : transactions
-- Ticket : CREDO-ALERT-001
-- Branch : main
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. ENUM-LIKE CHECK DOMAINS (avoid magic strings)
-- -----------------------------------------------------------------------------

-- status values
-- RECEIVED | APPROVED | DECLINED | REVIEW

-- transaction_type values
-- SALES | VOID | REFUND | REVERSAL

-- channel values
-- POS | PG

-- -----------------------------------------------------------------------------
-- 2. PARTITIONED BASE TABLE
-- -----------------------------------------------------------------------------

CREATE TABLE transactions (
    txn_id            UUID          NOT NULL,
    card_hash         VARCHAR(64)   NOT NULL,               -- SHA256(PAN + salt)
    masked_pan        VARCHAR(19),                          -- 411111XXXXXX1111 (display only)
    amount_minor      BIGINT        NOT NULL,                -- minor currency units (OMR 5.250 → 5250)
    currency          VARCHAR(3)    NOT NULL,               -- ISO 4217 e.g. OMR, INR
    merchant_id       VARCHAR(50),
    transaction_type  VARCHAR(20),                          -- SALES / VOID / REFUND / REVERSAL
    channel           VARCHAR(10),                          -- POS / PG
    scheme            VARCHAR(20),                          -- MASTERCARD / VISA
    rrn               VARCHAR(12),                          -- Retrieval Reference Number
    terminal_id       VARCHAR(50),
    mcc               VARCHAR(4),                           -- ISO 18245 Merchant Category Code
    txn_timestamp     TIMESTAMP     NOT NULL,
    status            VARCHAR(10),                          -- RECEIVED / APPROVED / DECLINED / REVIEW
    risk_score        INT           CHECK (risk_score BETWEEN 100 AND 1000),
    triggered_rules   JSONB,                                -- ["HIGH_AMOUNT", "VELOCITY_SPIKE"]
    processed_at      TIMESTAMP,                            -- fraud engine decision time (NULL until decided)
    created_at        TIMESTAMP     NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP     NOT NULL DEFAULT NOW(),

    PRIMARY KEY (txn_id, txn_timestamp)                    -- partition key must be in PK
)
PARTITION BY RANGE (txn_timestamp);

-- -----------------------------------------------------------------------------
-- 3. MONTHLY PARTITIONS
--    Add new partition at the start of each month via pg_partman or manually.
-- -----------------------------------------------------------------------------

CREATE TABLE transactions_2025_01 PARTITION OF transactions
    FOR VALUES FROM ('2025-01-01 00:00:00') TO ('2025-02-01 00:00:00');

CREATE TABLE transactions_2025_02 PARTITION OF transactions
    FOR VALUES FROM ('2025-02-01 00:00:00') TO ('2025-03-01 00:00:00');

CREATE TABLE transactions_2025_03 PARTITION OF transactions
    FOR VALUES FROM ('2025-03-01 00:00:00') TO ('2025-04-01 00:00:00');

CREATE TABLE transactions_2025_04 PARTITION OF transactions
    FOR VALUES FROM ('2025-04-01 00:00:00') TO ('2025-05-01 00:00:00');

CREATE TABLE transactions_2025_05 PARTITION OF transactions
    FOR VALUES FROM ('2025-05-01 00:00:00') TO ('2025-06-01 00:00:00');

CREATE TABLE transactions_2025_06 PARTITION OF transactions
    FOR VALUES FROM ('2025-06-01 00:00:00') TO ('2025-07-01 00:00:00');

CREATE TABLE transactions_2025_07 PARTITION OF transactions
    FOR VALUES FROM ('2025-07-01 00:00:00') TO ('2025-08-01 00:00:00');

CREATE TABLE transactions_2025_08 PARTITION OF transactions
    FOR VALUES FROM ('2025-08-01 00:00:00') TO ('2025-09-01 00:00:00');

CREATE TABLE transactions_2025_09 PARTITION OF transactions
    FOR VALUES FROM ('2025-09-01 00:00:00') TO ('2025-10-01 00:00:00');

CREATE TABLE transactions_2025_10 PARTITION OF transactions
    FOR VALUES FROM ('2025-10-01 00:00:00') TO ('2025-11-01 00:00:00');

CREATE TABLE transactions_2025_11 PARTITION OF transactions
    FOR VALUES FROM ('2025-11-01 00:00:00') TO ('2025-12-01 00:00:00');

CREATE TABLE transactions_2025_12 PARTITION OF transactions
    FOR VALUES FROM ('2025-12-01 00:00:00') TO ('2026-01-01 00:00:00');

-- Default partition catches anything outside defined ranges (safety net)
CREATE TABLE transactions_default PARTITION OF transactions DEFAULT;

-- 2026 partitions
CREATE TABLE transactions_2026_01 PARTITION OF transactions
    FOR VALUES FROM ('2026-01-01 00:00:00') TO ('2026-02-01 00:00:00');

CREATE TABLE transactions_2026_02 PARTITION OF transactions
    FOR VALUES FROM ('2026-02-01 00:00:00') TO ('2026-03-01 00:00:00');

CREATE TABLE transactions_2026_03 PARTITION OF transactions
    FOR VALUES FROM ('2026-03-01 00:00:00') TO ('2026-04-01 00:00:00');

CREATE TABLE transactions_2026_04 PARTITION OF transactions
    FOR VALUES FROM ('2026-04-01 00:00:00') TO ('2026-05-01 00:00:00');

CREATE TABLE transactions_2026_05 PARTITION OF transactions
    FOR VALUES FROM ('2026-05-01 00:00:00') TO ('2026-06-01 00:00:00');

CREATE TABLE transactions_2026_06 PARTITION OF transactions
    FOR VALUES FROM ('2026-06-01 00:00:00') TO ('2026-07-01 00:00:00');

CREATE TABLE transactions_2026_07 PARTITION OF transactions
    FOR VALUES FROM ('2026-07-01 00:00:00') TO ('2026-08-01 00:00:00');

CREATE TABLE transactions_2026_08 PARTITION OF transactions
    FOR VALUES FROM ('2026-08-01 00:00:00') TO ('2026-09-01 00:00:00');

CREATE TABLE transactions_2026_09 PARTITION OF transactions
    FOR VALUES FROM ('2026-09-01 00:00:00') TO ('2026-10-01 00:00:00');

CREATE TABLE transactions_2026_10 PARTITION OF transactions
    FOR VALUES FROM ('2026-10-01 00:00:00') TO ('2026-11-01 00:00:00');

CREATE TABLE transactions_2026_11 PARTITION OF transactions
    FOR VALUES FROM ('2026-11-01 00:00:00') TO ('2026-12-01 00:00:00');

CREATE TABLE transactions_2026_12 PARTITION OF transactions
    FOR VALUES FROM ('2026-12-01 00:00:00') TO ('2027-01-01 00:00:00');

-- -----------------------------------------------------------------------------
-- 4. INDEXES
--    Note: indexes on partitioned tables apply to all partitions automatically.
-- -----------------------------------------------------------------------------

-- Velocity lookups by card
CREATE INDEX idx_txn_card_hash       ON transactions (card_hash);

-- Date-range queries and partition pruning support
CREATE INDEX idx_txn_timestamp       ON transactions (txn_timestamp);

-- Reconciliation lookups by RRN
CREATE INDEX idx_txn_rrn             ON transactions (rrn);

-- Monitoring / ops queries by status
CREATE INDEX idx_txn_status          ON transactions (status);

-- Combined: per-card time-series queries (velocity, reporting)
CREATE INDEX idx_txn_card_time       ON transactions (card_hash, txn_timestamp DESC);

-- -----------------------------------------------------------------------------
-- 5. UPDATED_AT TRIGGER
--    DEFAULT NOW() only fires on INSERT. This trigger keeps updated_at current.
-- -----------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION fn_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- NOTE: Row-level triggers on a partitioned parent table require PostgreSQL 14+.
-- On PostgreSQL 10–13 this trigger must be created on each child partition individually.
CREATE TRIGGER trg_transactions_updated_at
    BEFORE UPDATE ON transactions
    FOR EACH ROW
    EXECUTE FUNCTION fn_set_updated_at();

-- -----------------------------------------------------------------------------
-- 6. UPSERT TEMPLATE (used by Persistence Service)
--    Insert full row from fraud_results enriched event.
--    On conflict (retry / reprocessing), update fraud decision fields only.
-- -----------------------------------------------------------------------------

-- INSERT INTO transactions (
--     txn_id, card_hash, masked_pan, amount_minor, currency,
--     merchant_id, transaction_type, channel, scheme, rrn,
--     terminal_id, mcc, txn_timestamp,
--     status, risk_score, triggered_rules, processed_at
-- )
-- VALUES (
--     $1, $2, $3, $4, $5,
--     $6, $7, $8, $9, $10,
--     $11, $12, $13,
--     $14, $15, $16, $17
-- )
-- ON CONFLICT (txn_id, txn_timestamp)
-- DO UPDATE SET
--     status          = EXCLUDED.status,
--     risk_score      = EXCLUDED.risk_score,
--     triggered_rules = EXCLUDED.triggered_rules,
--     processed_at    = EXCLUDED.processed_at,
--     updated_at      = NOW();
