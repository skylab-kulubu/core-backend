-- Refuses while a preference is kept: dropping the table would lose every
-- person's settings without their asking.
LOCK TABLE app_preferences IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM app_preferences) THEN
        RAISE EXCEPTION 'refusing to drop app_preferences while it holds preferences';
    END IF;
END
$$;

DROP TABLE IF EXISTS app_preferences;
DROP FUNCTION IF EXISTS public.app_preferences_refuse_inactive_subject();
