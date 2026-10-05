-- Rolling back drops the consent records: the grants themselves and the proof
-- of their withdrawals. Refuse while any row exists; export or ship a
-- forward repair instead.
LOCK TABLE contact_consents IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM contact_consents) THEN
        RAISE EXCEPTION 'contact_consents holds consent records (proof); refusing to drop it';
    END IF;
END
$$;

DROP TABLE IF EXISTS contact_consents;
