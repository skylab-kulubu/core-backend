-- SkyMail, the notification service, is SkyNotify now (ADR 0091). The values
-- core keeps under the old name take the new one:
--
--   * account_deletion_steps: the step erase_skymail is erase_skynotify, in
--     the step CHECK and in the checkpoint rows (their counts stay as they
--     are: completion proof).
--   * account_deletion_requests.last_error_code: erase_skymail_<rest>
--     (_failed, _rejected_403 …) is erase_skynotify_<rest>.
--   * certificate_mails.error_code: skymail_<rest> (_unreachable, _503 …) is
--     skynotify_<rest>.
--   * app_preferences.app: the namespace skymail is skynotify (ADR 0081; the
--     APP_PREFERENCE_CLIENTS value changes with the release). client_id is
--     the azp that wrote the row and stays: a record, not a name.
--   * the tables' comments that name SkyMail.
--
-- Audit records keep what they recorded. The release before this one does
-- not run on the new values: its erasure worker would write a step the CHECK
-- refuses. Rolling back runs db/tasima/skynotify-geri.sql (this down and the
-- deletion of this version's schema_migrations row) before the older image
-- starts; the newer image then runs this file again.
SET LOCAL lock_timeout = '5s';

-- Two rows of one person and key, one under each name, would collide.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM app_preferences old_name
        JOIN app_preferences new_name
          ON new_name.user_id = old_name.user_id AND new_name.key = old_name.key AND new_name.app = 'skynotify'
        WHERE old_name.app = 'skymail'
    ) THEN
        RAISE EXCEPTION 'app_preferences holds a key under both skymail and skynotify for one person; resolve it before renaming';
    END IF;
END
$$;

ALTER TABLE account_deletion_steps DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check;
UPDATE account_deletion_steps SET step = 'erase_skynotify' WHERE step = 'erase_skymail';
ALTER TABLE account_deletion_steps
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN (
            'disable_identity', 'logout_sessions',
            'erase_skynotify', 'erase_cms', 'erase_forms',
            'erase_contact_consents', 'erase_skypass_wallet', 'erase_communication_preferences',
            'erase_app_preferences',
            'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'
        ));

UPDATE account_deletion_requests
SET last_error_code = regexp_replace(last_error_code, '^erase_skymail_', 'erase_skynotify_')
WHERE last_error_code LIKE 'erase\_skymail\_%';

UPDATE certificate_mails
SET error_code = regexp_replace(error_code, '^skymail_', 'skynotify_')
WHERE error_code LIKE 'skymail\_%';

UPDATE app_preferences SET app = 'skynotify' WHERE app = 'skymail';

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
        WHERE c.relkind = 'r' AND d.description LIKE '%SkyMail%'
    LOOP
        IF described.objsubid = 0 THEN
            EXECUTE format('COMMENT ON TABLE %s IS %L', described.relation, replace(described.description, 'SkyMail', 'SkyNotify'));
        ELSE
            EXECUTE format('COMMENT ON COLUMN %s.%I IS %L', described.relation, described.attname, replace(described.description, 'SkyMail', 'SkyNotify'));
        END IF;
    END LOOP;
END
$$;
