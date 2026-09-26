-- The legacy purpose backfill (media redesign ticket 08).
--
-- Decision K2: a legacy Media the backfill gives a purpose may still be used
-- where core cannot see (CMS content shows Media by address). Until stage 5
-- gives those uses their Media attachments, such a Media keeps legacy's
-- detach rule: removed from its last record, it is detached with no expiry.
-- The backfill sets the hold with the purpose; releasing it (ticket 18)
-- starts the 30 days of the ones detached by then. Nothing else reads it:
-- archive, purge and account erasure ignore it.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS detach_expiry_held BOOLEAN NOT NULL DEFAULT false;

-- 20260926120000's status function, with the hold beside legacy.
CREATE OR REPLACE FUNCTION media_attachment_status()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    touched UUID[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT array_agg(DISTINCT media_id) INTO touched FROM new_attachments;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(DISTINCT media_id) INTO touched FROM old_attachments;
    ELSE
        SELECT array_agg(DISTINCT media_id) INTO touched
        FROM (SELECT media_id FROM old_attachments UNION SELECT media_id FROM new_attachments) moved;
    END IF;

    PERFORM 1 FROM media WHERE id = ANY (touched) ORDER BY id FOR NO KEY UPDATE;
    UPDATE media
    SET status = 'attached', expires_at = NULL, updated_at = now()
    WHERE id = ANY (touched)
      AND (status <> 'attached' OR expires_at IS NOT NULL)
      AND EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id);
    UPDATE media
    SET status = 'detached',
        expires_at = CASE WHEN purpose = 'legacy' OR detach_expiry_held THEN NULL ELSE now() + interval '30 days' END,
        updated_at = now()
    WHERE id = ANY (touched)
      AND status = 'attached'
      AND NOT EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id);
    RETURN NULL;
END;
$$;

-- Whether a Media of the purpose may play the product's role: the database's
-- copy of rolePurposes (internal/media/attachment.go), which a test keeps
-- equal to it. A legacy Media fits every role.
CREATE OR REPLACE FUNCTION media_purpose_fits_role(owner_service TEXT, role TEXT, purpose TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT purpose = 'legacy' OR (owner_service, role, purpose) IN (
        ('core', 'event_cover', 'event_cover'),
        ('core', 'event_cover', 'event_gallery'),
        ('core', 'event_gallery', 'event_gallery'),
        ('core', 'event_gallery', 'event_cover'),
        ('core', 'profile_picture', 'profile_picture'),
        ('core', 'certificate_asset', 'certificate_asset'),
        ('forms', 'answer', 'answer_file'),
        ('forms', 'answer', 'answer_file_large'),
        ('cms', 'image', 'cms_image'),
        ('cms', 'file', 'cms_file')
    )
$$;

-- A new Media attachment needs a current Media (20260926120000) whose
-- purpose fits the role. The link rules check the purpose before the write,
-- outside its transaction, so the backfill could give a legacy Media a
-- purpose in between. The Media row is read under the lock the foreign key
-- takes anyway: it waits for the backfill's lock and sees the purpose the
-- backfill wrote, and it never waits for the status trigger's lock, so two
-- links of one Media still do not wait for each other.
CREATE OR REPLACE FUNCTION require_current_attached_media()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    current_purpose TEXT;
BEGIN
    SELECT purpose INTO current_purpose FROM media
    WHERE id = NEW.media_id
      AND deleted_at IS NULL
      AND blob_purge_started_at IS NULL
      AND blob_purged_at IS NULL
    FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'media % is not current', NEW.media_id USING ERRCODE = '23503';
    END IF;
    IF NOT media_purpose_fits_role(NEW.owner_service, NEW.role, current_purpose) THEN
        RAISE EXCEPTION 'media % (%) does not fit the % role %', NEW.media_id, current_purpose, NEW.owner_service, NEW.role
            USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;
