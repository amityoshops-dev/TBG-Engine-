-- ============================================================================
-- TBG/CMS Core Ledger Schema
-- Strict append-only double-entry accounting: no UPDATE/DELETE on financial
-- history. Balances are always derived, never mutated directly.
-- ============================================================================

CREATE TABLE IF NOT EXISTS accounts (
    account_id   VARCHAR(64) PRIMARY KEY,
    account_name VARCHAR(128) NOT NULL,
    account_type VARCHAR(16)  NOT NULL CHECK (account_type IN ('ASSET','LIABILITY','SUSPENSE','EXPENSE')),
    currency     VARCHAR(3)   NOT NULL DEFAULT 'INR',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS journal_vouchers (
    jv_id              UUID PRIMARY KEY,
    idempotency_key    VARCHAR(128) UNIQUE NOT NULL,
    transaction_state  VARCHAR(32) NOT NULL DEFAULT 'POSTED_INTERNAL',
    prev_hash          VARCHAR(64) NOT NULL,
    current_hash       VARCHAR(64) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS postings (
    posting_id  BIGSERIAL PRIMARY KEY,
    jv_id       UUID NOT NULL REFERENCES journal_vouchers(jv_id),
    account_id  VARCHAR(64) NOT NULL REFERENCES accounts(account_id),
    direction   VARCHAR(2) NOT NULL CHECK (direction IN ('DR','CR')),
    amount      NUMERIC(18,4) NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Revoke UPDATE/DELETE at the role level to hard-enforce append-only semantics
-- (adjust role name to match your deployment's application DB user).
-- REVOKE UPDATE, DELETE ON postings, journal_vouchers FROM app_user;

CREATE INDEX IF NOT EXISTS idx_postings_account ON postings(account_id);
CREATE INDEX IF NOT EXISTS idx_postings_jv ON postings(jv_id);

-- ----------------------------------------------------------------------------
-- Zero-sum invariant trigger: sum(DR) - sum(CR) = 0 for every journal voucher.
-- ----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION verify_jv_balance() RETURNS TRIGGER AS $$
DECLARE
    net_bal NUMERIC(18,4);
BEGIN
    SELECT COALESCE(SUM(CASE WHEN direction = 'DR' THEN amount ELSE -amount END), 0)
    INTO net_bal
    FROM postings
    WHERE jv_id = NEW.jv_id;

    IF net_bal <> 0 THEN
        RAISE EXCEPTION 'Double-entry invariant violated for jv_id %: net balance is %, must equal 0.0000', NEW.jv_id, net_bal;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_verify_jv_balance ON postings;
CREATE CONSTRAINT TRIGGER trg_verify_jv_balance
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION verify_jv_balance();

-- ----------------------------------------------------------------------------
-- Seed accounts referenced throughout the engine and README examples.
-- ----------------------------------------------------------------------------
INSERT INTO accounts (account_id, account_name, account_type, currency) VALUES
    ('00040310001928',               'Corporate Operating Float (Demo Client)', 'LIABILITY', 'INR'),
    ('AC_CMS_SUSPENSE_CLEARING_9999','CMS Intraday Clearing Suspense',          'SUSPENSE',  'INR'),
    ('AC_RBI_NOSTRO_0001',           'Central Bank Settlement Nostro',          'ASSET',     'INR')
ON CONFLICT (account_id) DO NOTHING;
