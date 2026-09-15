-- =============================================================================
-- SentinelSwitch — Migration 002
-- Table  : transactions
-- Adds   : decision column
--
-- The Persistence Service upsert (services/persistence-svc/internal/store/postgres.go)
-- writes fraud decision into both `status` (current processing state) and
-- `decision` (immutable fraud-engine verdict: APPROVED / DECLINED / REVIEW).
-- Migration 001 only created `status` — this adds the missing `decision` column.
--
-- ALTER TABLE ADD COLUMN on a partitioned parent cascades to all partitions.
-- =============================================================================

ALTER TABLE transactions ADD COLUMN decision VARCHAR(10);
