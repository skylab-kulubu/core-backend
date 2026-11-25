SET LOCAL lock_timeout = '5s';

-- Co-organizers of an Event (docs/co-organizers.md): the teams and people
-- who manage the Event beside its one Owner team. A row is a team (team_code,
-- a Team code like events.owner_team) or a person (user_id, an e-skylab
-- account), never both.
--
--   * management: who of the team manages, leaders (its Leaders alone) or
--     members (every member, as the GECEKODU Owner team rule); a team row
--     always says it, a person row never.
--   * visibility: co_organizer (named on the site, the Ticket page and the
--     certificate check) or helper (seen in the panel alone); a team row
--     always says it, a person row never. A person is never shown outside.
--   * added_by: who added the row, an id alone; its name is read when shown.
--
-- Only PUT /v1/events/{id}/organizers writes the rows; the Event goes with
-- them. A person's rights end 30 days after the Event; that is derived, not
-- stored. The release before reads nor writes the table.
CREATE TABLE IF NOT EXISTS event_co_organizers (
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    team_code TEXT,
    user_id UUID,
    management TEXT,
    visibility TEXT,
    added_by UUID,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT event_co_organizers_one_kind_check CHECK ((team_code IS NULL) <> (user_id IS NULL)),
    CONSTRAINT event_co_organizers_choices_check
        CHECK ((team_code IS NOT NULL) = (management IS NOT NULL AND visibility IS NOT NULL)
            AND (user_id IS NULL OR (management IS NULL AND visibility IS NULL))),
    CONSTRAINT event_co_organizers_management_check CHECK (management IS NULL OR management IN ('leaders', 'members')),
    CONSTRAINT event_co_organizers_visibility_check CHECK (visibility IS NULL OR visibility IN ('co_organizer', 'helper')),
    CONSTRAINT event_co_organizers_team_code_check CHECK (team_code IS NULL OR btrim(team_code) <> ''),
    CONSTRAINT event_co_organizers_event_team_key UNIQUE (event_id, team_code),
    CONSTRAINT event_co_organizers_event_user_key UNIQUE (event_id, user_id)
);
COMMENT ON TABLE event_co_organizers IS
    'Co-organizers (docs/co-organizers.md): teams and people who manage an Event beside its Owner team.';

-- A person's rows (account erasure, retention) and a team's (its Events,
-- the Team code rename check).
CREATE INDEX IF NOT EXISTS event_co_organizers_user_idx ON event_co_organizers (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS event_co_organizers_team_idx ON event_co_organizers (team_code) WHERE team_code IS NOT NULL;

-- user_id and added_by are current-identity links, as event_door_staff's
-- user_id: only an active subject with no deletion marker may be named.
DROP TRIGGER IF EXISTS event_co_organizers_require_active_subject ON event_co_organizers;
CREATE TRIGGER event_co_organizers_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON event_co_organizers
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id');
DROP TRIGGER IF EXISTS event_co_organizers_require_active_adder ON event_co_organizers;
CREATE TRIGGER event_co_organizers_require_active_adder
    BEFORE INSERT OR UPDATE OF added_by ON event_co_organizers
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('added_by');
