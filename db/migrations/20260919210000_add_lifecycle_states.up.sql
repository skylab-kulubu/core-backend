ALTER TABLE events
    ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS archived_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS events_current_owner_team_idx
    ON events (owner_team)
    WHERE archived_at IS NULL;

ALTER TABLE event_days
    ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS archived_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS event_days_current_event_idx
    ON event_days (event_id)
    WHERE archived_at IS NULL;

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS archived_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS sessions_current_event_day_idx
    ON sessions (event_day_id)
    WHERE archived_at IS NULL;

ALTER TABLE seasons
    ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS archived_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS seasons_current_start_date_idx
    ON seasons (start_date)
    WHERE archived_at IS NULL;

ALTER TABLE competitors
    ADD COLUMN IF NOT EXISTS withdrawn_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS withdrawn_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS competitors_current_event_idx
    ON competitors (event_id)
    WHERE withdrawn_at IS NULL;

CREATE INDEX IF NOT EXISTS competitors_current_user_idx
    ON competitors (user_id)
    WHERE withdrawn_at IS NULL;

ALTER TABLE media
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS deleted_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS media_current_created_at_idx
    ON media (created_at DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE urls
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disabled_by UUID REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS urls_current_created_by_idx
    ON urls (created_by)
    WHERE disabled_at IS NULL;
