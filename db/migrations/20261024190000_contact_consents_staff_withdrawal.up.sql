-- A withdrawal the person asked for by mail to info@yildizskylab.com
-- (aydınlatma s6 §6), carried out by staff with `core-backend
-- consent-withdraw` (docs/contact-consents.md): the grants it ends say so
-- (ended_via staff), and that row is the record of the request kept.
ALTER TABLE contact_consents DROP CONSTRAINT IF EXISTS contact_consents_ended_check;
ALTER TABLE contact_consents ADD CONSTRAINT contact_consents_ended_check CHECK (
    (ended_at IS NULL) = (ended_reason IS NULL)
    AND (ended_at IS NULL) = (ended_via IS NULL)
    AND (ended_reason IS NULL OR ended_reason IN ('withdrawn', 'expired', 'superseded'))
    AND (ended_via IS NULL OR ended_via IN ('link', 'one_click', 'self', 'service', 'renewal_unanswered', 'staff'))
);
