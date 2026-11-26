-- Rolls back an Event's ticket image. Its Media attachments go with the
-- table (the triggers go first, so the rows are deleted here); the
-- uploaded images become detached and expire. Every band is cut from the
-- cover's middle again. The role tables lose the role and keep every other
-- row.
SET LOCAL lock_timeout = '5s';

DROP TRIGGER IF EXISTS event_ticket_images_attachments_insert ON event_ticket_images;
DROP TRIGGER IF EXISTS event_ticket_images_attachments_update ON event_ticket_images;
DROP TRIGGER IF EXISTS event_ticket_images_attachments_delete ON event_ticket_images;
DELETE FROM media_attachments WHERE owner_service = 'core' AND owner_type = 'event' AND role = 'event_ticket_image';
DROP TABLE IF EXISTS event_ticket_images;

DO $migration$
DECLARE
    purpose_rows TEXT;
    strict_rows TEXT;
BEGIN
    SELECT string_agg(format('(%L, %L, %L)', p.owner_service, p.role, p.purpose), E',\n        ' ORDER BY p.n)
    INTO purpose_rows
    FROM media_role_purposes() WITH ORDINALITY AS p (owner_service, role, purpose, n)
    WHERE p.role <> 'event_ticket_image';
    EXECUTE format($f$
CREATE OR REPLACE FUNCTION media_role_purposes()
RETURNS TABLE (owner_service TEXT, role TEXT, purpose TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, purpose_rows);

    SELECT string_agg(format('(%L, %L)', s.owner_service, s.role), E',\n        ' ORDER BY s.n)
    INTO strict_rows
    FROM media_roles_without_legacy() WITH ORDINALITY AS s (owner_service, role, n)
    WHERE s.role <> 'event_ticket_image';
    EXECUTE format($f$
CREATE OR REPLACE FUNCTION media_roles_without_legacy()
RETURNS TABLE (owner_service TEXT, role TEXT)
LANGUAGE sql
IMMUTABLE
AS $body$
    VALUES
        %s
$body$$f$, strict_rows);
END
$migration$;
