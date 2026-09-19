DROP INDEX IF EXISTS urls_current_created_by_idx;
DROP INDEX IF EXISTS media_current_created_at_idx;
DROP INDEX IF EXISTS competitors_current_user_idx;
DROP INDEX IF EXISTS competitors_current_event_idx;
DROP INDEX IF EXISTS seasons_current_start_date_idx;
DROP INDEX IF EXISTS sessions_current_event_day_idx;
DROP INDEX IF EXISTS event_days_current_event_idx;
DROP INDEX IF EXISTS events_current_owner_team_idx;

ALTER TABLE urls
    DROP COLUMN IF EXISTS disabled_by,
    DROP COLUMN IF EXISTS disabled_at;

ALTER TABLE media
    DROP COLUMN IF EXISTS deleted_by,
    DROP COLUMN IF EXISTS deleted_at;

ALTER TABLE competitors
    DROP COLUMN IF EXISTS withdrawn_by,
    DROP COLUMN IF EXISTS withdrawn_at;

ALTER TABLE seasons
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;

ALTER TABLE sessions
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;

ALTER TABLE event_days
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;

ALTER TABLE events
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;
