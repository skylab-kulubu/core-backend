-- Drops events.active (ADR 0066). Since 20261012130000 active is derived
-- from the status and the dates (internal/event/status.go) and the column is
-- read by nothing; since the release before this one nothing writes it
-- either, so that release, rolled back to, still works on this schema.
--
-- DROP COLUMN only marks the column dropped in the catalog: no rewrite, no
-- scan. It takes events' strongest lock for that instant; behind one waiting
-- for a long transaction every Event read would queue. Waiting at most 5
-- seconds instead, the migration fails, this release does not start, the one
-- before keeps serving, and the next start applies it.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events DROP COLUMN IF EXISTS active;
