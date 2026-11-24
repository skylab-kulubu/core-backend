-- Why a manual issue was given (docs/certificate-rule.md): required when
-- the Ticket is below the certificate rule, kept when given to an eligible
-- one. With requested_by and created_at it is the manual issue's audit
-- record. NULL for every batch written before, and for the other reasons.
ALTER TABLE certificate_batches ADD COLUMN IF NOT EXISTS justification TEXT;
ALTER TABLE certificate_batches DROP CONSTRAINT IF EXISTS certificate_batches_justification_check;
ALTER TABLE certificate_batches ADD CONSTRAINT certificate_batches_justification_check
    CHECK (justification IS NULL OR char_length(justification) BETWEEN 1 AND 500);
