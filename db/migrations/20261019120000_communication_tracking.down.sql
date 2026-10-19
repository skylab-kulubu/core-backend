-- Refuses while it would lose something: a person's objection (dropping it
-- would track them again), a withdrawal SkyMail has not heard of yet, or
-- the audit trail of the objections.
LOCK TABLE communication_preferences IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM communication_preferences WHERE scope = 'tracking') THEN
        RAISE EXCEPTION 'refusing to drop the tracking preference while a person has one';
    END IF;
    IF to_regclass('public.communication_tracking_withdrawals') IS NOT NULL
       AND EXISTS (SELECT 1 FROM communication_tracking_withdrawals) THEN
        RAISE EXCEPTION 'refusing to drop communication_tracking_withdrawals while a withdrawal waits for SkyMail';
    END IF;
    IF to_regclass('public.communication_tracking_changes') IS NOT NULL
       AND EXISTS (SELECT 1 FROM communication_tracking_changes) THEN
        RAISE EXCEPTION 'refusing to drop communication_tracking_changes while it holds the audit trail';
    END IF;
END
$$;

DROP TABLE IF EXISTS communication_tracking_changes;
DROP TABLE IF EXISTS communication_tracking_withdrawals;

ALTER TABLE communication_preferences DROP CONSTRAINT IF EXISTS communication_preferences_category_check;
ALTER TABLE communication_preferences ADD CONSTRAINT communication_preferences_category_check CHECK (
    (scope = 'all' AND group_id IS NULL AND news_kind IS NULL)
    OR (scope = 'club' AND group_id IS NULL AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
    OR (scope = 'team' AND group_id IS NOT NULL AND length(group_id) BETWEEN 1 AND 64
        AND news_kind IS NOT NULL AND news_kind IN ('duyuru', 'haber'))
);
