-- Every Media has a Media purpose (ADR-0052). Media stored before purposes
-- existed, and every purpose-less upload, are `legacy`. The purposes
-- themselves live in config/media-purposes.json, not in the schema.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'legacy';
