-- The Team audit record (docs/team-audit.md, ADR 0065): every change core
-- makes to a Group (create, rename, attributes, members, role mappings)
-- through its routes. A write opens its row as pending before it asks
-- Keycloak and writes the outcome after; a row that cannot be opened stops
-- the write. Rows are written by internal/teamaudit only.
--
-- Ids, Group paths, codes and role names only: no name, no e-mail address,
-- no free personal data (before/after carry the recorded attributes and
-- role names, cut to their lengths). No foreign key to users: the actor and
-- the target are Keycloak subjects, and account erasure leaves the rows as
-- they are (an erased subject resolves to nobody). The retention sweep
-- deletes rows three years old (team_audit_age).
CREATE TABLE IF NOT EXISTS team_audit_records (
    id                 UUID        PRIMARY KEY,
    occurred_at        TIMESTAMPTZ NOT NULL,
    actor_id           UUID        NOT NULL,
    actor_basis        TEXT        NOT NULL,
    actor_via_group_id UUID,
    action             TEXT        NOT NULL,
    group_id           UUID,
    group_path         TEXT        NOT NULL,
    target_user_id     UUID,
    before             JSONB,
    after              JSONB,
    outcome            TEXT        NOT NULL,
    error_code         TEXT,
    correlation_id     TEXT        NOT NULL DEFAULT '',
    client             TEXT        NOT NULL DEFAULT '',
    CONSTRAINT team_audit_records_actor_basis_check CHECK (
        (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
        OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)),
    CONSTRAINT team_audit_records_action_check CHECK (action IN (
        'member.add', 'member.remove', 'leader.add', 'leader.remove',
        'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
        'group.roles', 'group.attributes')),
    CONSTRAINT team_audit_records_outcome_check CHECK (outcome IN ('pending', 'ok', 'failed', 'denied')),
    -- Only a create that Keycloak has not confirmed lacks the Group's id.
    CONSTRAINT team_audit_records_group_id_check CHECK (
        group_id IS NOT NULL OR (action = 'team.create' AND outcome <> 'ok')),
    CONSTRAINT team_audit_records_group_path_check CHECK (group_path LIKE '/%' AND char_length(group_path) <= 1024),
    CONSTRAINT team_audit_records_target_check CHECK (
        (action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove')) = (target_user_id IS NOT NULL)),
    CONSTRAINT team_audit_records_state_check CHECK (
        (before IS NULL OR (jsonb_typeof(before) = 'object' AND octet_length(before::text) <= 32768))
        AND (after IS NULL OR (jsonb_typeof(after) = 'object' AND octet_length(after::text) <= 32768))),
    -- A code for failed and denied, none otherwise.
    CONSTRAINT team_audit_records_error_code_check CHECK (
        (outcome IN ('failed', 'denied')) = (error_code IS NOT NULL)
        AND (error_code IS NULL OR error_code ~ '^[a-z0-9_]{1,64}$')),
    CONSTRAINT team_audit_records_correlation_id_check CHECK (char_length(correlation_id) <= 128),
    CONSTRAINT team_audit_records_client_check CHECK (char_length(client) <= 255)
);

COMMENT ON TABLE team_audit_records IS
    'Team audit record (docs/team-audit.md): who changed which Group, when, and how. Ids, paths, codes and role names; no name or e-mail. Kept three years (retention rule team_audit_age); account erasure does not touch it.';
COMMENT ON COLUMN team_audit_records.actor_id IS
    'The actor''s Keycloak subject. No name or e-mail is kept; names are read from core''s users when the record is shown.';
COMMENT ON COLUMN team_audit_records.target_user_id IS
    'The person of a member or leader action, as a Keycloak subject. No name or e-mail is kept.';
COMMENT ON COLUMN team_audit_records.before IS
    'The Group before the change: id, path, parent, code, recorded attributes (names, descriptions, logo media id, flags), names of other attributes, role names. No name or e-mail of a person.';
COMMENT ON COLUMN team_audit_records.after IS
    'The Group after the change, as before. No name or e-mail of a person.';
COMMENT ON COLUMN team_audit_records.client IS
    'The client the actor''s token was issued to (azp).';

CREATE INDEX IF NOT EXISTS team_audit_records_group_idx ON team_audit_records (group_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS team_audit_records_actor_idx ON team_audit_records (actor_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS team_audit_records_target_idx ON team_audit_records (target_user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS team_audit_records_occurred_idx ON team_audit_records (occurred_at);

-- A row is written once and finished once: only a pending row changes, and
-- only its outcome, error code, after and (for a create) Group id. Nothing
-- else updates a row; the retention sweep deletes it.
CREATE OR REPLACE FUNCTION team_audit_records_finish_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.outcome <> 'pending'
        OR NEW.outcome = 'pending'
        OR NEW.id IS DISTINCT FROM OLD.id
        OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at
        OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
        OR NEW.actor_basis IS DISTINCT FROM OLD.actor_basis
        OR NEW.actor_via_group_id IS DISTINCT FROM OLD.actor_via_group_id
        OR NEW.action IS DISTINCT FROM OLD.action
        OR (OLD.group_id IS NOT NULL AND NEW.group_id IS DISTINCT FROM OLD.group_id)
        OR NEW.group_path IS DISTINCT FROM OLD.group_path
        OR NEW.target_user_id IS DISTINCT FROM OLD.target_user_id
        OR NEW.before IS DISTINCT FROM OLD.before
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.client IS DISTINCT FROM OLD.client THEN
        RAISE EXCEPTION 'team_audit_records: a row only changes from pending to its outcome'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS team_audit_records_finish_only ON team_audit_records;
CREATE TRIGGER team_audit_records_finish_only
    BEFORE UPDATE ON team_audit_records
    FOR EACH ROW EXECUTE FUNCTION team_audit_records_finish_only();
