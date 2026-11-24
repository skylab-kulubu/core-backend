-- Keycloak console changes in the Team audit record (docs/console-audit.md,
-- grup-yonetimi 10, dalga 2 A6). core reads Keycloak's admin events and
-- writes each change made outside core (the console, or the Admin REST API
-- by anyone but core) as a finished row: via console, basis console, the
-- actor Keycloak names. Each admin event once: keycloak_event_id.
--
-- New actions: user.roles (a role mapped to a person directly; no Group,
-- the person as target, like user.*) and keycloak.client / keycloak.role
-- (Keycloak configuration: no Group, no target).
--
-- console_audit_cursor is where the reader stands in Keycloak's admin
-- events (one row). privilege_notice_mails.via marks a notice of a console
-- change, so the mail names the console as where it was made.
--
-- The release before this one runs on the new schema: it writes none of
-- the new values, never reads the cursor, and sends a notice row as before
-- (without the console's name).
SET LOCAL lock_timeout = '5s';

ALTER TABLE team_audit_records ADD COLUMN IF NOT EXISTS keycloak_event_id TEXT;
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_keycloak_event_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_keycloak_event_id_check CHECK (
    (keycloak_event_id IS NULL) = (via IS DISTINCT FROM 'console')
    AND (keycloak_event_id IS NULL OR keycloak_event_id ~ '^[A-Za-z0-9._:-]{1,64}$'));
CREATE UNIQUE INDEX IF NOT EXISTS team_audit_records_keycloak_event_idx
    ON team_audit_records (keycloak_event_id) WHERE keycloak_event_id IS NOT NULL;
COMMENT ON COLUMN team_audit_records.keycloak_event_id IS
    'The Keycloak admin event a via console row records (its id); NULL for every change core made.';

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_via_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_via_check
    CHECK (via IS NULL OR via IN ('invitation', 'access_request', 'access_review', 'console'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile', 'user.roles',
    'keycloak.client', 'keycloak.role'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action = 'membership.term.expired')
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview'))
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));

-- keycloak.* rows carry neither a Group nor a person; user.roles is a
-- user.* action and already fits the user.* rules.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL
    OR (action = 'team.create' AND outcome <> 'ok')
    OR (action IN ('app.assign', 'app.unassign') AND target_user_id IS NOT NULL)
    OR (action LIKE 'user.%' AND target_user_id IS NOT NULL)
    OR action LIKE 'keycloak.%');

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    (action NOT LIKE 'user.%' AND action NOT LIKE 'keycloak.%' AND group_path LIKE '/%' AND char_length(group_path) <= 1024)
    OR (action IN ('app.assign', 'app.unassign') AND group_path = '' AND group_id IS NULL)
    OR ((action LIKE 'user.%' OR action LIKE 'keycloak.%') AND group_path = '' AND group_id IS NULL));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_target_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_target_check CHECK (
    CASE
        WHEN action IN ('member.add', 'member.remove', 'leader.add', 'leader.remove',
            'membership.term.extend', 'membership.term.handover', 'membership.term.expired') THEN target_user_id IS NOT NULL
        WHEN action IN ('app.assign', 'app.unassign') THEN (target_user_id IS NULL) <> (group_id IS NULL)
        WHEN action LIKE 'user.%' THEN target_user_id IS NOT NULL AND group_id IS NULL
        ELSE target_user_id IS NULL
    END);

-- The finish-only trigger stays: a console row is written ok and never
-- changes. It also keeps keycloak_event_id from changing.
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
        OR NEW.client IS DISTINCT FROM OLD.client
        OR NEW.via IS DISTINCT FROM OLD.via
        OR NEW.keycloak_event_id IS DISTINCT FROM OLD.keycloak_event_id THEN
        RAISE EXCEPTION 'team_audit_records: a row only changes from pending to its outcome'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS console_audit_cursor (
    id          SMALLINT    PRIMARY KEY DEFAULT 1,
    event_time  BIGINT      NOT NULL,
    event_ids   TEXT[]      NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT console_audit_cursor_one_row CHECK (id = 1),
    CONSTRAINT console_audit_cursor_time_check CHECK (event_time >= 0),
    CONSTRAINT console_audit_cursor_ids_check CHECK (cardinality(event_ids) <= 1000)
);
COMMENT ON TABLE console_audit_cursor IS
    'Where core''s reader of Keycloak admin events stands (docs/console-audit.md): the newest event time read (epoch ms) and the ids read at that time. No personal data.';

ALTER TABLE privilege_notice_mails ADD COLUMN IF NOT EXISTS via TEXT;
ALTER TABLE privilege_notice_mails DROP CONSTRAINT IF EXISTS privilege_notice_mails_via_check;
ALTER TABLE privilege_notice_mails ADD CONSTRAINT privilege_notice_mails_via_check CHECK (via IS NULL OR via = 'console');
COMMENT ON COLUMN privilege_notice_mails.via IS
    'console: the change was made in Keycloak''s console (docs/console-audit.md); NULL for one made through core.';
