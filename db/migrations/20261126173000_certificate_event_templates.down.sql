SET LOCAL lock_timeout = '5s';

-- Rolling back turns every Event template into a team template of its
-- Event's Owner team (owner_team already holds it); their Event bindings
-- stay. The release before neither writes nor reads the column, so rolling
-- back the code alone needs no down migration.
DROP INDEX IF EXISTS certificate_templates_one_per_event;
ALTER TABLE certificate_templates DROP COLUMN IF EXISTS event_id;
