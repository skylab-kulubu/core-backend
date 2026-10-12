-- Competitions (ADR-0068): a Competition belongs to one Event, which gives
-- it its Owner team, Season and Tickets; nothing here stores ownership
-- again. Each Competition has Stages, each with one Scoring strategy. An
-- Entrant is a person or a team whose members are Tickets of the Event,
-- registered or guest. Scores are computed from an append-only ledger:
-- a row is never changed or deleted, a correction revokes it with a reason
-- and writes a new one. Points are thousandths (BIGINT).
--
-- New tables only: nothing that runs today reads or writes them, so the
-- release before and after this migration serve side by side. The old
-- competitors table is untouched (ticket 06 retires it).

CREATE TABLE IF NOT EXISTS competitions (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id),
    name TEXT NOT NULL CHECK (btrim(name) <> '' AND char_length(name) <= 200),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 5000),
    entrant_mode TEXT NOT NULL DEFAULT 'individual'
        CHECK (entrant_mode IN ('individual', 'team', 'both')),
    status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'open', 'running', 'closed', 'cancelled')),
    cancel_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(cancel_reason) <= 1000),
    created_by UUID REFERENCES users (id) ON DELETE SET NULL,
    archived_at TIMESTAMPTZ,
    archived_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT competitions_cancelled_has_reason
        CHECK ((status = 'cancelled') = (btrim(cancel_reason) <> ''))
);

CREATE INDEX IF NOT EXISTS competitions_event_idx ON competitions (event_id, created_at);

CREATE TABLE IF NOT EXISTS competition_stages (
    id UUID PRIMARY KEY,
    competition_id UUID NOT NULL REFERENCES competitions (id),
    position INTEGER NOT NULL CHECK (position >= 1),
    name TEXT NOT NULL DEFAULT '' CHECK (char_length(name) <= 200),
    strategy TEXT NOT NULL CHECK (strategy ~ '^[a-z][a-z_]{0,31}$'),
    config JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    config_version INTEGER NOT NULL DEFAULT 1 CHECK (config_version >= 1),
    starts_at TIMESTAMPTZ,
    ends_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (starts_at IS NULL OR ends_at IS NULL OR ends_at > starts_at),
    CONSTRAINT competition_stages_position_key UNIQUE (competition_id, position),
    CONSTRAINT competition_stages_competition_key UNIQUE (id, competition_id)
);

CREATE TABLE IF NOT EXISTS competition_entrants (
    id UUID PRIMARY KEY,
    competition_id UUID NOT NULL REFERENCES competitions (id),
    kind TEXT NOT NULL DEFAULT 'individual' CHECK (kind IN ('individual', 'team')),
    team_name TEXT NOT NULL DEFAULT '' CHECK (char_length(team_name) <= 200),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'withdrawn', 'disqualified')),
    status_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(status_reason) <= 1000),
    created_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (kind = 'team' OR team_name = ''),
    CONSTRAINT competition_entrants_competition_key UNIQUE (id, competition_id)
);

CREATE INDEX IF NOT EXISTS competition_entrants_competition_idx
    ON competition_entrants (competition_id, created_at);

-- A Ticket is in at most one Entrant of a Competition.
CREATE TABLE IF NOT EXISTS competition_entrant_members (
    entrant_id UUID NOT NULL,
    competition_id UUID NOT NULL,
    ticket_id UUID NOT NULL REFERENCES tickets (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entrant_id, ticket_id),
    FOREIGN KEY (entrant_id, competition_id) REFERENCES competition_entrants (id, competition_id),
    CONSTRAINT competition_entrant_members_ticket_key UNIQUE (competition_id, ticket_id)
);

CREATE INDEX IF NOT EXISTS competition_entrant_members_ticket_idx
    ON competition_entrant_members (ticket_id);

-- A member is a Ticket of the Competition's own Event.
CREATE OR REPLACE FUNCTION public.competition_entrant_member_same_event()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM public.tickets t
        JOIN public.competitions c ON c.event_id = t.event_id
        WHERE t.id = NEW.ticket_id AND c.id = NEW.competition_id
    ) THEN
        RAISE EXCEPTION 'an entrant member is a ticket of the competition''s event'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_entrant_member_same_event';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS competition_entrant_members_same_event ON competition_entrant_members;
CREATE TRIGGER competition_entrant_members_same_event
    BEFORE INSERT OR UPDATE ON competition_entrant_members
    FOR EACH ROW EXECUTE FUNCTION public.competition_entrant_member_same_event();

-- The ledger. source: manual (an organizer typed it), native (made on the
-- platform), import:<batch> (ticket 12). occurred_at is the contest's
-- time, recorded_at when it was written.
CREATE TABLE IF NOT EXISTS competition_submissions (
    id UUID PRIMARY KEY,
    competition_id UUID NOT NULL,
    stage_id UUID NOT NULL,
    entrant_id UUID NOT NULL,
    source TEXT NOT NULL
        CHECK (source IN ('manual', 'native') OR source ~ '^import:[0-9a-f-]{36}$'),
    value BIGINT CHECK (value BETWEEN -1000000000000 AND 1000000000000),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_by UUID REFERENCES users (id) ON DELETE SET NULL,
    replaces_id UUID REFERENCES competition_submissions (id),
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users (id) ON DELETE SET NULL,
    revoke_reason TEXT CHECK (revoke_reason IS NULL OR (btrim(revoke_reason) <> '' AND char_length(revoke_reason) <= 1000)),
    CHECK (source <> 'manual' OR value IS NOT NULL),
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CHECK (revoked_at IS NOT NULL OR revoked_by IS NULL),
    FOREIGN KEY (stage_id, competition_id) REFERENCES competition_stages (id, competition_id),
    FOREIGN KEY (entrant_id, competition_id) REFERENCES competition_entrants (id, competition_id)
);

CREATE INDEX IF NOT EXISTS competition_submissions_stage_idx
    ON competition_submissions (stage_id, recorded_at);

-- One live manual score per Entrant and Stage: setting another revokes it.
CREATE UNIQUE INDEX IF NOT EXISTS competition_submissions_live_manual_idx
    ON competition_submissions (stage_id, entrant_id)
    WHERE source = 'manual' AND revoked_at IS NULL;

-- A row is replaced at most once.
CREATE UNIQUE INDEX IF NOT EXISTS competition_submissions_replaces_idx
    ON competition_submissions (replaces_id)
    WHERE replaces_id IS NOT NULL;

-- Penalties, bonuses, hint costs and tie-break decisions, each with its
-- reason (spec §5.5, §5.8). No route writes them yet.
CREATE TABLE IF NOT EXISTS competition_adjustments (
    id UUID PRIMARY KEY,
    competition_id UUID NOT NULL,
    stage_id UUID NOT NULL,
    entrant_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('penalty', 'bonus', 'hint', 'tiebreak')),
    value BIGINT NOT NULL CHECK (value BETWEEN -1000000000000 AND 1000000000000),
    reason TEXT NOT NULL CHECK (btrim(reason) <> '' AND char_length(reason) <= 1000),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_by UUID REFERENCES users (id) ON DELETE SET NULL,
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users (id) ON DELETE SET NULL,
    revoke_reason TEXT CHECK (revoke_reason IS NULL OR (btrim(revoke_reason) <> '' AND char_length(revoke_reason) <= 1000)),
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CHECK (revoked_at IS NOT NULL OR revoked_by IS NULL),
    FOREIGN KEY (stage_id, competition_id) REFERENCES competition_stages (id, competition_id),
    FOREIGN KEY (entrant_id, competition_id) REFERENCES competition_entrants (id, competition_id)
);

CREATE INDEX IF NOT EXISTS competition_adjustments_stage_idx
    ON competition_adjustments (stage_id, recorded_at);

-- Ledger rows are never deleted and never edited. The one change a row
-- takes is its revocation, once; and an actor column may be cleared when
-- the account is purged (ON DELETE SET NULL).
CREATE OR REPLACE FUNCTION public.competition_ledger_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    changeable CONSTANT TEXT[] := ARRAY['revoked_at', 'revoked_by', 'revoke_reason', 'recorded_by'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'competition ledger rows are never deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_ledger_append_only';
    END IF;
    IF (to_jsonb(NEW) - changeable) IS DISTINCT FROM (to_jsonb(OLD) - changeable)
        OR (NEW.recorded_by IS DISTINCT FROM OLD.recorded_by AND NEW.recorded_by IS NOT NULL)
        OR (OLD.revoked_at IS NOT NULL AND (
            NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
            OR NEW.revoke_reason IS DISTINCT FROM OLD.revoke_reason
            OR (NEW.revoked_by IS DISTINCT FROM OLD.revoked_by AND NEW.revoked_by IS NOT NULL)))
    THEN
        RAISE EXCEPTION 'competition ledger rows are only revoked, once'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_ledger_append_only';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS competition_submissions_append_only ON competition_submissions;
CREATE TRIGGER competition_submissions_append_only
    BEFORE UPDATE OR DELETE ON competition_submissions
    FOR EACH ROW EXECUTE FUNCTION public.competition_ledger_append_only();

DROP TRIGGER IF EXISTS competition_adjustments_append_only ON competition_adjustments;
CREATE TRIGGER competition_adjustments_append_only
    BEFORE UPDATE OR DELETE ON competition_adjustments
    FOR EACH ROW EXECUTE FUNCTION public.competition_ledger_append_only();

-- Actor links follow every other subject link (account lifecycle): no new
-- link to an account that is being deleted. One trigger per column: when
-- a purged account's links are cleared (ON DELETE SET NULL, one column at
-- a time), the other column of the row is not checked again.
DROP TRIGGER IF EXISTS competitions_require_active_creator ON competitions;
CREATE TRIGGER competitions_require_active_creator
    BEFORE INSERT OR UPDATE OF created_by ON competitions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('created_by');

DROP TRIGGER IF EXISTS competitions_require_active_archiver ON competitions;
CREATE TRIGGER competitions_require_active_archiver
    BEFORE INSERT OR UPDATE OF archived_by ON competitions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('archived_by');

DROP TRIGGER IF EXISTS competition_entrants_require_active_creator ON competition_entrants;
CREATE TRIGGER competition_entrants_require_active_creator
    BEFORE INSERT OR UPDATE OF created_by ON competition_entrants
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('created_by');

DROP TRIGGER IF EXISTS competition_submissions_require_active_recorder ON competition_submissions;
CREATE TRIGGER competition_submissions_require_active_recorder
    BEFORE INSERT OR UPDATE OF recorded_by ON competition_submissions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('recorded_by');

DROP TRIGGER IF EXISTS competition_submissions_require_active_revoker ON competition_submissions;
CREATE TRIGGER competition_submissions_require_active_revoker
    BEFORE INSERT OR UPDATE OF revoked_by ON competition_submissions
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('revoked_by');

DROP TRIGGER IF EXISTS competition_adjustments_require_active_recorder ON competition_adjustments;
CREATE TRIGGER competition_adjustments_require_active_recorder
    BEFORE INSERT OR UPDATE OF recorded_by ON competition_adjustments
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('recorded_by');

DROP TRIGGER IF EXISTS competition_adjustments_require_active_revoker ON competition_adjustments;
CREATE TRIGGER competition_adjustments_require_active_revoker
    BEFORE INSERT OR UPDATE OF revoked_by ON competition_adjustments
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('revoked_by');
