-- Registration closure (ADR 0066 addendum 2026-10-19, ticket 12): an
-- Event's applying closed by hand or by a deadline, apart from its status.
--
--   * registration_closed: the organizer closed applying by hand.
--   * registration_closes_at: applying closes at this moment (none: no
--     deadline).
--   * registration_closed_at: when it was closed by hand, so that a forms
--     answer can be judged by when it was sent (Yusuf, 2026-10-20).
--
-- Whether applying is open is derived (internal/event/status.go): published,
-- not ended, not closed by hand and before the deadline. Every existing
-- Event stays open.
--
-- The release before this one keeps working on the new schema: its writes
-- name their columns, so an Event it creates or updates takes or keeps the
-- defaults, and nothing it reads changes.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events
    ADD COLUMN IF NOT EXISTS registration_closed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS registration_closes_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS registration_closed_at TIMESTAMPTZ;
