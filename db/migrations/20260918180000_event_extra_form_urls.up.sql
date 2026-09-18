ALTER TABLE events
    ADD COLUMN IF NOT EXISTS extra_form_urls JSONB NOT NULL DEFAULT '[]'::jsonb;
