CREATE TABLE IF NOT EXISTS account_deletion_self_intakes (
    request_id UUID NOT NULL,
    idempotency_key_hash BYTEA NOT NULL,
    receipt_lookup_hash BYTEA NOT NULL,
    receipt_hash BYTEA NOT NULL,
    receipt_expires_at TIMESTAMPTZ NOT NULL,
    receipt_revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE account_deletion_self_intakes
    ADD COLUMN IF NOT EXISTS request_id UUID,
    ADD COLUMN IF NOT EXISTS idempotency_key_hash BYTEA,
    ADD COLUMN IF NOT EXISTS receipt_lookup_hash BYTEA,
    ADD COLUMN IF NOT EXISTS receipt_hash BYTEA,
    ADD COLUMN IF NOT EXISTS receipt_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS receipt_revoked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT now(),
    ALTER COLUMN request_id SET NOT NULL,
    ALTER COLUMN idempotency_key_hash SET NOT NULL,
    ALTER COLUMN receipt_lookup_hash SET NOT NULL,
    ALTER COLUMN receipt_hash SET NOT NULL,
    ALTER COLUMN receipt_expires_at SET NOT NULL,
    ALTER COLUMN created_at SET NOT NULL,
    ALTER COLUMN created_at SET DEFAULT now(),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_pkey,
    ADD CONSTRAINT account_deletion_self_intakes_pkey PRIMARY KEY (request_id),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_request_id_fkey,
    ADD CONSTRAINT account_deletion_self_intakes_request_id_fkey
        FOREIGN KEY (request_id) REFERENCES account_deletion_requests (id),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_idempotency_key_hash_key,
    ADD CONSTRAINT account_deletion_self_intakes_idempotency_key_hash_key UNIQUE (idempotency_key_hash),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_receipt_lookup_hash_key,
    ADD CONSTRAINT account_deletion_self_intakes_receipt_lookup_hash_key UNIQUE (receipt_lookup_hash),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_idempotency_key_hash_check,
    ADD CONSTRAINT account_deletion_self_intakes_idempotency_key_hash_check
        CHECK (octet_length(idempotency_key_hash) = 32),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_receipt_lookup_hash_check,
    ADD CONSTRAINT account_deletion_self_intakes_receipt_lookup_hash_check
        CHECK (octet_length(receipt_lookup_hash) = 32),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_receipt_hash_check,
    ADD CONSTRAINT account_deletion_self_intakes_receipt_hash_check
        CHECK (octet_length(receipt_hash) = 32),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_expiry_check,
    ADD CONSTRAINT account_deletion_self_intakes_expiry_check
        CHECK (receipt_expires_at > created_at),
    DROP CONSTRAINT IF EXISTS account_deletion_self_intakes_revocation_check,
    ADD CONSTRAINT account_deletion_self_intakes_revocation_check
        CHECK (receipt_revoked_at IS NULL OR receipt_revoked_at >= created_at);

DROP INDEX IF EXISTS account_deletion_self_intakes_expiry_idx;
CREATE INDEX account_deletion_self_intakes_expiry_idx
    ON account_deletion_self_intakes (receipt_expires_at, request_id)
    WHERE receipt_revoked_at IS NULL;

COMMENT ON TABLE account_deletion_self_intakes IS
    'Hash-only, bounded and revocable self-service deletion idempotency/status capabilities. Raw keys and receipts must never be stored.';
