SET LOCAL lock_timeout = '5s';

-- Event templates (docs/certificate-event-templates.md): a certificate
-- template may belong to one Event, which it is bound to only. NULL is a
-- team or club template, as every row before this migration. Nullable and
-- without a default: adding the column rewrites nothing. Events are never
-- deleted (they are archived), so RESTRICT only guards a future delete.
ALTER TABLE certificate_templates
    ADD COLUMN IF NOT EXISTS event_id UUID REFERENCES events (id) ON DELETE RESTRICT;

-- One current Event template per Event; an archived one leaves room for a
-- new one. The table holds a few hundred rows at most, so the index builds
-- under the lock in well under the timeout.
CREATE UNIQUE INDEX IF NOT EXISTS certificate_templates_one_per_event
    ON certificate_templates (event_id) WHERE event_id IS NOT NULL AND archived_at IS NULL;

COMMENT ON COLUMN certificate_templates.event_id IS
    'The Event an Event template belongs to and is bound to only; NULL for a team or club template.';
