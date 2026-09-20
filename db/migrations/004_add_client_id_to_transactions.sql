-- =============================================================================
-- SentinelSwitch — Migration 004
-- Table  : transactions
-- Adds   : client_id column
--
-- Needed to implement GetTransactionStatus (services/api-gateway) safely: a
-- status lookup by txn_id alone would let any authenticated caller query any
-- other caller's transaction, defeating the multi-tenant isolation built for
-- Kafka result delivery (see docs/MULTI_TENANT_RESULT_DELIVERY.md). Persisting
-- client_id lets the lookup filter by (txn_id, client_id) so a caller can only
-- ever see their own transactions.
--
-- Persistence Service (services/persistence-svc/internal/store/postgres.go)
-- now writes FraudResultEvent.client_id into this column on every upsert.
--
-- ALTER TABLE ADD COLUMN on a partitioned parent cascades to all partitions.
-- =============================================================================

ALTER TABLE transactions ADD COLUMN client_id VARCHAR(64);

-- Supports the (txn_id, client_id) lookup GetTransactionStatus performs.
CREATE INDEX idx_txn_client_id ON transactions (client_id);
