-- Rolling back gives the values their SkyMail names again, so that the
-- release before this one runs: the step erase_skymail in the CHECK and the
-- checkpoint rows, the erase_skymail_* and skymail_* error codes, the app
-- namespace skymail and the tables' comments. db/tasima/skynotify-geri.sql
-- is this file and the deletion of this version's schema_migrations row.
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM app_preferences new_name
        JOIN app_preferences old_name
          ON old_name.user_id = new_name.user_id AND old_name.key = new_name.key AND old_name.app = 'skymail'
        WHERE new_name.app = 'skynotify'
    ) THEN
        RAISE EXCEPTION 'app_preferences holds a key under both skynotify and skymail for one person; resolve it before renaming';
    END IF;
END
$$;

ALTER TABLE account_deletion_steps DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check;
UPDATE account_deletion_steps SET step = 'erase_skymail' WHERE step = 'erase_skynotify';
ALTER TABLE account_deletion_steps
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skymail', 'erase_cms', 'erase_forms',
            'erase_contact_consents', 'erase_skypass_wallet', 'erase_communication_preferences',
            'erase_app_preferences',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));

UPDATE account_deletion_requests
SET last_error_code = regexp_replace(last_error_code, '^erase_skynotify_', 'erase_skymail_')
WHERE last_error_code LIKE 'erase\_skynotify\_%';

UPDATE certificate_mails
SET error_code = regexp_replace(error_code, '^skynotify_', 'skymail_')
WHERE error_code LIKE 'skynotify\_%';

UPDATE app_preferences SET app = 'skymail' WHERE app = 'skynotify';

DO $$
DECLARE
    described RECORD;
BEGIN
    FOR described IN
        SELECT c.oid::regclass AS relation, d.objsubid, a.attname, d.description
        FROM pg_description d
        JOIN pg_class c ON c.oid = d.objoid AND d.classoid = 'pg_class'::regclass
        JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
        LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = d.objsubid
        WHERE c.relkind = 'r' AND d.description LIKE '%SkyNotify%'
    LOOP
        IF described.objsubid = 0 THEN
            EXECUTE format('COMMENT ON TABLE %s IS %L', described.relation, replace(described.description, 'SkyNotify', 'SkyMail'));
        ELSE
            EXECUTE format('COMMENT ON COLUMN %s.%I IS %L', described.relation, described.attname, replace(described.description, 'SkyNotify', 'SkyMail'));
        END IF;
    END LOOP;
END
$$;
