-- Account deletion markers are the anti-resurrection source of truth. Once a
-- request exists (including after hard purge), or an account left active state,
-- rolling this migration back would allow an old JWT to recreate the identity.
-- Intentionally fail instead of weakening that guarantee or fabricating owners.
-- Lock every table that can create or erase a subject reference before the
-- preflight. This closes the check/drop race with a live deletion request or
-- an in-flight subject-link writer.
-- Guarded writers acquire their leaf table before the trigger reads users and
-- the deletion marker. Lock in that same direction; marker-first ordering can
-- deadlock a writer that already owns (for example) tickets.
LOCK TABLE tickets, competitors, media, certificates, url_hits, urls,
    event_door_staff, certificate_template_versions, certificate_template_bindings,
    certificate_event_state, certificate_batches, certificate_templates,
    media_upload_staging, sessions, event_days, seasons, events, event_images,
    users, account_deletion_requests,
    account_deletion_steps, account_deletion_outbox
    IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM account_deletion_requests)
       OR EXISTS (SELECT 1 FROM users WHERE account_state <> 'active')
       OR EXISTS (SELECT 1 FROM media WHERE uploaded_by IS NULL)
       OR EXISTS (SELECT 1 FROM competitors WHERE user_id IS NULL)
       OR EXISTS (SELECT 1 FROM media_upload_staging) THEN
        RAISE EXCEPTION 'account lifecycle rollback would remove anti-resurrection state; restore a pre-migration backup instead';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS tickets_require_active_owner ON tickets;
DROP TRIGGER IF EXISTS competitors_require_active_subject ON competitors;
DROP TRIGGER IF EXISTS media_require_active_uploader ON media;
DROP TRIGGER IF EXISTS media_upload_staging_require_active_subject ON media_upload_staging;
DROP TRIGGER IF EXISTS certificates_require_active_owner ON certificates;
DROP TRIGGER IF EXISTS urls_require_active_creator ON urls;
DROP TRIGGER IF EXISTS url_hits_require_active_subject ON url_hits;
DROP TRIGGER IF EXISTS event_door_staff_require_active_subject ON event_door_staff;
DROP TRIGGER IF EXISTS events_require_active_archiver ON events;
DROP TRIGGER IF EXISTS event_days_require_active_archiver ON event_days;
DROP TRIGGER IF EXISTS sessions_require_active_archiver ON sessions;
DROP TRIGGER IF EXISTS seasons_require_active_archiver ON seasons;
DROP TRIGGER IF EXISTS competitors_require_active_withdrawer ON competitors;
DROP TRIGGER IF EXISTS media_require_active_deleter ON media;
DROP TRIGGER IF EXISTS urls_require_active_disabler ON urls;
DROP TRIGGER IF EXISTS certificate_templates_require_active_creator ON certificate_templates;
DROP TRIGGER IF EXISTS certificate_template_versions_require_active_publisher ON certificate_template_versions;
DROP TRIGGER IF EXISTS certificate_template_bindings_require_active_updater ON certificate_template_bindings;
DROP TRIGGER IF EXISTS certificate_event_state_require_active_finalizer ON certificate_event_state;
DROP TRIGGER IF EXISTS certificate_batches_require_active_requester ON certificate_batches;
DROP TRIGGER IF EXISTS account_deletion_requests_require_active_subject ON account_deletion_requests;
DROP TRIGGER IF EXISTS account_deletion_requests_require_active_requester ON account_deletion_requests;
DROP FUNCTION IF EXISTS public.require_active_account_reference();

DROP INDEX IF EXISTS media_upload_staging_cleanup_idx;
DROP INDEX IF EXISTS media_upload_staging_subject_idx;
DROP TABLE IF EXISTS media_upload_staging;

ALTER TABLE media
    DROP CONSTRAINT IF EXISTS media_uploaded_by_fkey,
    ADD CONSTRAINT media_uploaded_by_fkey
        FOREIGN KEY (uploaded_by) REFERENCES users (id) ON DELETE CASCADE,
    ALTER COLUMN uploaded_by SET NOT NULL;

ALTER TABLE competitors
    DROP CONSTRAINT IF EXISTS competitors_user_id_fkey,
    ADD CONSTRAINT competitors_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    ALTER COLUMN user_id SET NOT NULL;

DROP INDEX IF EXISTS account_deletion_outbox_pending_idx;
DROP TABLE IF EXISTS account_deletion_outbox;
DROP TABLE IF EXISTS account_deletion_steps;
DROP INDEX IF EXISTS account_deletion_requests_claim_idx;
DROP TABLE IF EXISTS account_deletion_requests;

DROP INDEX IF EXISTS users_account_state_idx;

ALTER TABLE users
    DROP COLUMN IF EXISTS anonymized_at,
    DROP COLUMN IF EXISTS deletion_requested_at,
    DROP COLUMN IF EXISTS account_state;
