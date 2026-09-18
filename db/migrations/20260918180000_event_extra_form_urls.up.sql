ALTER TABLE events
    ADD COLUMN extra_form_urls JSONB NOT NULL DEFAULT '[]'::jsonb;
