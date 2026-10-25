-- Puts back the check without the staff way. It is added NOT VALID so that
-- the grants staff already ended stay: they are consent records (proof).
-- PostgreSQL checks a NOT VALID CHECK on every row an UPDATE changes, and
-- an ended row is not updated again.
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_ended_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_ended_check CHECK (
    (ended_at IS NULL) = (ended_reason IS NULL)
    AND (ended_at IS NULL) = (ended_via IS NULL)
    AND (ended_reason IS NULL OR ended_reason IN ('withdrawn', 'expired', 'superseded'))
    AND (ended_via IS NULL OR ended_via IN ('link', 'one_click', 'self', 'service', 'renewal_unanswered'))
) NOT VALID;
