CREATE OR REPLACE FUNCTION require_current_attached_media()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM media
        WHERE id = NEW.media_id
          AND deleted_at IS NULL
          AND blob_purge_started_at IS NULL
          AND blob_purged_at IS NULL
    ) THEN
        RAISE EXCEPTION 'media % is not current', NEW.media_id USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION IF EXISTS media_purpose_fits_role(TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS media_role_purposes();

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
        expires_at = CASE WHEN purpose = 'legacy' THEN NULL ELSE now() + interval '30 days' END,
        updated_at = now()
    WHERE id = ANY (touched)
      AND status = 'attached'
      AND NOT EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id);
    RETURN NULL;
END;
$$;

DROP TABLE IF EXISTS media_legacy_hold;

ALTER TABLE media
    DROP COLUMN IF EXISTS detach_expiry_held;
