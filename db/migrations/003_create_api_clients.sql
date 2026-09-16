-- =============================================================================
-- SentinelSwitch — Migration 003
-- Table  : api_clients
-- Purpose: API-key registry for authenticating external callers/integrators
--          on the API Gateway's SubmitTransaction gRPC call.
--
-- client_id here is distinct from merchant_id (transactions.merchant_id):
--   - client_id  = who is calling the API (the integrator/platform)
--   - merchant_id = which merchant a given transaction is for
-- One client can submit transactions for many merchants.
-- =============================================================================

CREATE TABLE api_clients (
    client_id      VARCHAR(64)  PRIMARY KEY,
    api_key_hash   CHAR(64)     NOT NULL UNIQUE,   -- sha256(api_key), hex
    api_key_prefix VARCHAR(12),                    -- display/support only, e.g. "sk_live_51H" — NEVER the full key
    name           VARCHAR(100) NOT NULL,
    status         VARCHAR(10)  NOT NULL DEFAULT 'active', -- active | suspended
    created_at     TIMESTAMP    NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMP    NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_api_clients_status CHECK (status IN ('active', 'suspended'))
);

CREATE INDEX idx_api_clients_status ON api_clients (status);

-- Reuses the same updated_at trigger function created in migration 001.
CREATE TRIGGER trg_api_clients_updated_at
    BEFORE UPDATE ON api_clients
    FOR EACH ROW
    EXECUTE FUNCTION fn_set_updated_at();

-- NOTE: docker-entrypoint-initdb.d only runs against a fresh Postgres volume.
-- On an already-initialized dev volume, apply manually:
--   docker exec -i sentinel-postgres psql -U sentinel -d sentinelswitch -f - < db/migrations/003_create_api_clients.sql
