-- Refuses while a preference is kept: dropping the table would mail every
-- person who turned a category off again.
LOCK TABLE mail_preferences IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM mail_preferences) THEN
        RAISE EXCEPTION 'refusing to drop mail_preferences while it holds preferences';
    END IF;
END
$$;

DROP TABLE IF EXISTS mail_preferences;
DROP FUNCTION IF EXISTS public.mail_preferences_refuse_inactive_subject();
