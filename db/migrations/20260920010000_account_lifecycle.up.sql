ALTER TABLE users
    ADD COLUMN IF NOT EXISTS account_state TEXT NOT NULL DEFAULT 'active'
        CHECK (account_state IN ('active', 'deletion_pending', 'anonymized')),
    ADD COLUMN IF NOT EXISTS deletion_requested_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS anonymized_at TIMESTAMPTZ;

ALTER TABLE users
    ALTER COLUMN account_state SET DEFAULT 'active',
    ALTER COLUMN account_state SET NOT NULL,
    DROP CONSTRAINT IF EXISTS users_account_state_check,
    ADD CONSTRAINT users_account_state_check
        CHECK (account_state IN ('active', 'deletion_pending', 'anonymized'));

DROP INDEX IF EXISTS users_account_state_idx;
CREATE INDEX users_account_state_idx ON users (account_state, updated_at);

ALTER TABLE competitors
    ALTER COLUMN user_id DROP NOT NULL,
    DROP CONSTRAINT IF EXISTS competitors_user_id_fkey,
    ADD CONSTRAINT competitors_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE SET NULL;

ALTER TABLE media
    ALTER COLUMN uploaded_by DROP NOT NULL,
    DROP CONSTRAINT IF EXISTS media_uploaded_by_fkey,
    ADD CONSTRAINT media_uploaded_by_fkey
        FOREIGN KEY (uploaded_by) REFERENCES users (id) ON DELETE SET NULL;

-- Uploads are registered here before the object write. The media row and the
-- removal of this intent are committed atomically; abandoned/rejected object
-- writes therefore remain discoverable after a process crash.
CREATE TABLE IF NOT EXISTS media_upload_staging (
    object_key TEXT PRIMARY KEY,
    subject_id UUID NOT NULL,
    cleanup_after TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE media_upload_staging
    ADD COLUMN IF NOT EXISTS subject_id UUID,
    ALTER COLUMN subject_id SET NOT NULL,
    DROP CONSTRAINT IF EXISTS media_upload_staging_attempt_count_check,
    ADD CONSTRAINT media_upload_staging_attempt_count_check CHECK (attempt_count >= 0);

DROP INDEX IF EXISTS media_upload_staging_cleanup_idx;
CREATE INDEX media_upload_staging_cleanup_idx
    ON media_upload_staging (cleanup_after, created_at);

DROP INDEX IF EXISTS media_upload_staging_subject_idx;
CREATE INDEX media_upload_staging_subject_idx
    ON media_upload_staging (subject_id, cleanup_after, created_at);

CREATE TABLE IF NOT EXISTS account_deletion_requests (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL UNIQUE,
    requested_by UUID REFERENCES users (id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'completed', 'manual_intervention')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    lease_token UUID,
    profile_media_id UUID,
    last_error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

ALTER TABLE account_deletion_requests
    ADD COLUMN IF NOT EXISTS lease_token UUID,
    ADD COLUMN IF NOT EXISTS profile_media_id UUID,
    DROP CONSTRAINT IF EXISTS account_deletion_requests_subject_id_key,
    ADD CONSTRAINT account_deletion_requests_subject_id_key UNIQUE (subject_id),
    DROP CONSTRAINT IF EXISTS account_deletion_requests_status_check,
    ADD CONSTRAINT account_deletion_requests_status_check
        CHECK (status IN ('pending', 'processing', 'completed', 'manual_intervention')),
    DROP CONSTRAINT IF EXISTS account_deletion_requests_attempt_count_check,
    ADD CONSTRAINT account_deletion_requests_attempt_count_check CHECK (attempt_count >= 0);

DROP INDEX IF EXISTS account_deletion_requests_claim_idx;
CREATE INDEX account_deletion_requests_claim_idx
    ON account_deletion_requests (next_attempt_at, created_at)
    WHERE status IN ('pending', 'processing');

CREATE TABLE IF NOT EXISTS account_deletion_steps (
    request_id UUID NOT NULL REFERENCES account_deletion_requests (id) ON DELETE CASCADE,
    step TEXT NOT NULL
        CHECK (step IN ('disable_identity', 'logout_sessions', 'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity')),
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, step)
);

ALTER TABLE account_deletion_steps
    DROP CONSTRAINT IF EXISTS account_deletion_steps_step_check,
    ADD CONSTRAINT account_deletion_steps_step_check
        CHECK (step IN ('disable_identity', 'logout_sessions', 'anonymize_core', 'erase_profile_media', 'erase_staged_uploads', 'delete_identity'));

CREATE TABLE IF NOT EXISTS account_deletion_outbox (
    id UUID PRIMARY KEY,
    request_id UUID NOT NULL UNIQUE REFERENCES account_deletion_requests (id) ON DELETE CASCADE,
    subject_id UUID NOT NULL,
    event_type TEXT NOT NULL DEFAULT 'account.deletion_requested'
        CHECK (event_type = 'account.deletion_requested'),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE account_deletion_outbox
    DROP CONSTRAINT IF EXISTS account_deletion_outbox_event_type_check,
    ADD CONSTRAINT account_deletion_outbox_event_type_check CHECK (event_type = 'account.deletion_requested'),
    DROP CONSTRAINT IF EXISTS account_deletion_outbox_attempt_count_check,
    ADD CONSTRAINT account_deletion_outbox_attempt_count_check CHECK (attempt_count >= 0);

DROP INDEX IF EXISTS account_deletion_outbox_pending_idx;
CREATE INDEX account_deletion_outbox_pending_idx
    ON account_deletion_outbox (available_at, created_at)
    WHERE published_at IS NULL;

COMMENT ON TABLE account_deletion_outbox IS
    'Durable producer contract for the cross-service account erasure orchestrator; the Core worker does not publish these rows.';

-- Every durable subject link shares the same transaction-scoped advisory lock
-- as account-deletion request creation. A writer that wins the lock commits
-- before anonymization (and is therefore detached by it); a writer that loses
-- observes the deletion marker and is rejected. This also protects callers
-- that bypass the Go service layer.
CREATE OR REPLACE FUNCTION public.require_active_account_reference()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    column_name TEXT;
    candidate_subject UUID;
    subject_ids UUID[] := ARRAY[]::UUID[];
BEGIN
    FOREACH column_name IN ARRAY TG_ARGV LOOP
        candidate_subject := NULLIF(to_jsonb(NEW)->>column_name, '')::UUID;
        IF candidate_subject IS NOT NULL THEN
            subject_ids := array_append(subject_ids, candidate_subject);
        END IF;
    END LOOP;

    SELECT COALESCE(array_agg(DISTINCT value ORDER BY value), ARRAY[]::UUID[])
    INTO subject_ids
    FROM unnest(subject_ids) AS value;

    FOREACH candidate_subject IN ARRAY subject_ids LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(candidate_subject::TEXT, 6001410475649093715));
        IF NOT EXISTS (
            SELECT 1 FROM public.users WHERE id = candidate_subject AND account_state = 'active'
        ) OR EXISTS (
            SELECT 1 FROM public.account_deletion_requests WHERE subject_id = candidate_subject
        ) THEN
            RAISE EXCEPTION 'account subject is not active'
                USING ERRCODE = '23514', CONSTRAINT = 'active_account_reference';
        END IF;
    END LOOP;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS tickets_require_active_owner ON tickets;
CREATE TRIGGER tickets_require_active_owner
    BEFORE INSERT OR UPDATE OF owner_id ON tickets
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('owner_id');

DROP TRIGGER IF EXISTS competitors_require_active_subject ON competitors;
DROP TRIGGER IF EXISTS competitors_require_active_withdrawer ON competitors;
CREATE TRIGGER competitors_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id, withdrawn_at, withdrawn_by ON competitors
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id', 'withdrawn_by');

DROP TRIGGER IF EXISTS media_require_active_uploader ON media;
CREATE TRIGGER media_require_active_uploader
    BEFORE INSERT OR UPDATE OF uploaded_by ON media
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('uploaded_by');

DROP TRIGGER IF EXISTS media_upload_staging_require_active_subject ON media_upload_staging;
CREATE TRIGGER media_upload_staging_require_active_subject
    BEFORE INSERT OR UPDATE OF subject_id ON media_upload_staging
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('subject_id');

DROP TRIGGER IF EXISTS certificates_require_active_owner ON certificates;
CREATE TRIGGER certificates_require_active_owner
    BEFORE INSERT OR UPDATE OF owner_id ON certificates
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('owner_id');

DROP TRIGGER IF EXISTS urls_require_active_creator ON urls;
CREATE TRIGGER urls_require_active_creator
    BEFORE INSERT OR UPDATE OF created_by ON urls
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('created_by');

DROP TRIGGER IF EXISTS url_hits_require_active_subject ON url_hits;
CREATE TRIGGER url_hits_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON url_hits
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id');

DROP TRIGGER IF EXISTS event_door_staff_require_active_subject ON event_door_staff;
CREATE TRIGGER event_door_staff_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON event_door_staff
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id');

DROP TRIGGER IF EXISTS events_require_active_archiver ON events;
CREATE TRIGGER events_require_active_archiver
    BEFORE INSERT OR UPDATE OF archived_by ON events
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('archived_by');

DROP TRIGGER IF EXISTS event_days_require_active_archiver ON event_days;
CREATE TRIGGER event_days_require_active_archiver
    BEFORE INSERT OR UPDATE OF archived_by ON event_days
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('archived_by');

DROP TRIGGER IF EXISTS sessions_require_active_archiver ON sessions;
CREATE TRIGGER sessions_require_active_archiver
    BEFORE INSERT OR UPDATE OF archived_by ON sessions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('archived_by');

DROP TRIGGER IF EXISTS seasons_require_active_archiver ON seasons;
CREATE TRIGGER seasons_require_active_archiver
    BEFORE INSERT OR UPDATE OF archived_by ON seasons
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('archived_by');

DROP TRIGGER IF EXISTS media_require_active_deleter ON media;
CREATE TRIGGER media_require_active_deleter
    BEFORE INSERT OR UPDATE OF deleted_by ON media
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('deleted_by');

DROP TRIGGER IF EXISTS urls_require_active_disabler ON urls;
CREATE TRIGGER urls_require_active_disabler
    BEFORE INSERT OR UPDATE OF disabled_by ON urls
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('disabled_by');

DROP TRIGGER IF EXISTS certificate_templates_require_active_creator ON certificate_templates;
CREATE TRIGGER certificate_templates_require_active_creator
    BEFORE INSERT OR UPDATE OF created_by ON certificate_templates
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('created_by');

DROP TRIGGER IF EXISTS certificate_template_versions_require_active_publisher ON certificate_template_versions;
CREATE TRIGGER certificate_template_versions_require_active_publisher
    BEFORE INSERT OR UPDATE OF published_by ON certificate_template_versions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('published_by');

DROP TRIGGER IF EXISTS certificate_template_bindings_require_active_updater ON certificate_template_bindings;
CREATE TRIGGER certificate_template_bindings_require_active_updater
    BEFORE INSERT OR UPDATE OF updated_by ON certificate_template_bindings
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('updated_by');

DROP TRIGGER IF EXISTS certificate_event_state_require_active_finalizer ON certificate_event_state;
CREATE TRIGGER certificate_event_state_require_active_finalizer
    BEFORE INSERT OR UPDATE OF attendance_finalized_by ON certificate_event_state
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('attendance_finalized_by');

DROP TRIGGER IF EXISTS certificate_batches_require_active_requester ON certificate_batches;
CREATE TRIGGER certificate_batches_require_active_requester
    BEFORE INSERT OR UPDATE OF requested_by ON certificate_batches
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('requested_by');

DROP TRIGGER IF EXISTS account_deletion_requests_require_active_subject ON account_deletion_requests;
CREATE TRIGGER account_deletion_requests_require_active_subject
    BEFORE INSERT ON account_deletion_requests
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('subject_id', 'requested_by');

DROP TRIGGER IF EXISTS account_deletion_requests_require_active_requester ON account_deletion_requests;
CREATE TRIGGER account_deletion_requests_require_active_requester
    BEFORE UPDATE OF requested_by ON account_deletion_requests
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('requested_by');
