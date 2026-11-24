-- Drops the manual issues' justifications: the text written since is lost.
ALTER TABLE certificate_batches
    DROP CONSTRAINT IF EXISTS certificate_batches_justification_check,
    DROP COLUMN IF EXISTS justification;
