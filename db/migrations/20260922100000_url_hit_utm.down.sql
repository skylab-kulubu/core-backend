ALTER TABLE url_hits
    DROP COLUMN IF EXISTS utm_content,
    DROP COLUMN IF EXISTS utm_term,
    DROP COLUMN IF EXISTS utm_campaign,
    DROP COLUMN IF EXISTS utm_medium,
    DROP COLUMN IF EXISTS utm_source;
