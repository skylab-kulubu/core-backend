-- Refuses while a preference is kept: dropping the table would mail every
-- person who turned a category off again.
LOCK TABLE communication_preferences IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM communication_preferences) THEN
        RAISE EXCEPTION 'refusing to drop communication_preferences while it holds preferences';
    END IF;
END
$$;

DROP TABLE IF EXISTS communication_preferences;
DROP FUNCTION IF EXISTS public.communication_preferences_refuse_inactive_subject();
