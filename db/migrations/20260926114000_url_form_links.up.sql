ALTER TABLE urls
    ADD COLUMN IF NOT EXISTS form_id UUID,
    ADD COLUMN IF NOT EXISTS event_id UUID REFERENCES events (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS label TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS urls_current_form_idx
    ON urls (form_id)
    WHERE form_id IS NOT NULL AND disabled_at IS NULL;

CREATE INDEX IF NOT EXISTS urls_event_idx ON urls (event_id) WHERE event_id IS NOT NULL;

WITH event_forms AS (
    SELECT DISTINCT ON (link.form_id) link.form_id, link.event_id, link.alias, link.label
    FROM (
        SELECT e.id AS event_id,
               e.name AS label,
               substring(e.form_url FROM '[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}')::uuid AS form_id,
               CASE WHEN jsonb_typeof(e.extra_form_urls) = 'object' THEN e.extra_form_urls ->> 'applyAlias' END AS alias,
               e.created_at
        FROM events e
        WHERE e.archived_at IS NULL
        UNION ALL
        SELECT e.id,
               CASE WHEN coalesce(x ->> 'label', '') = '' THEN e.name ELSE e.name || ' · ' || (x ->> 'label') END,
               substring(x ->> 'url' FROM '[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}')::uuid,
               x ->> 'alias',
               e.created_at
        FROM events e
        CROSS JOIN LATERAL jsonb_array_elements(
            CASE jsonb_typeof(e.extra_form_urls)
                WHEN 'array' THEN e.extra_form_urls
                WHEN 'object' THEN coalesce(e.extra_form_urls -> 'extra', '[]'::jsonb)
                ELSE '[]'::jsonb
            END
        ) AS x
        WHERE e.archived_at IS NULL
    ) link
    WHERE link.form_id IS NOT NULL AND coalesce(link.alias, '') <> ''
    ORDER BY link.form_id, link.created_at DESC
)
UPDATE urls u
SET form_id = ef.form_id, event_id = ef.event_id, label = ef.label
FROM event_forms ef
WHERE u.alias = ef.alias
  AND u.disabled_at IS NULL
  AND u.form_id IS NULL
  AND position(ef.form_id::text IN lower(u.url)) > 0;

WITH candidates AS (
    SELECT DISTINCT ON (c.form_id) c.id, c.form_id
    FROM (
        SELECT u.id,
               u.click_count,
               u.created_at,
               substring(u.url FROM '^https?://[^/?#]*forms[^/?#]*/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/?(?:[?#].*)?$')::uuid AS form_id
        FROM urls u
        WHERE u.disabled_at IS NULL AND u.form_id IS NULL
    ) c
    WHERE c.form_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM urls b WHERE b.form_id = c.form_id AND b.disabled_at IS NULL)
    ORDER BY c.form_id, c.click_count DESC, c.created_at ASC
)
UPDATE urls u
SET form_id = c.form_id
FROM candidates c
WHERE u.id = c.id;
