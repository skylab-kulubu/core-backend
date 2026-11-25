SET LOCAL lock_timeout = '5s';

-- An Event's Co-organizer list in the Team audit record and its mails
-- (docs/co-organizers.md, ticket 05).
--
-- The Team audit record takes co_organizer.add, co_organizer.remove and
-- co_organizer.change. A row's target is the Event's Owner team; an Event
-- without one has no Group, so such a row carries none (group_id NULL,
-- group_path ''); one with a path has its Group. The other checks stay
-- 20261126110000's.
--
-- co_organizer_notifications is the mails' queue: while
-- CO_ORGANIZER_MAIL is on, PUT /v1/events/{id}/organizers queues a row for
-- each team or person added or removed in the transaction that writes the
-- list; a worker sends it two minutes later if it still holds (an added one
-- still on the list, a removed one still off it) and deletes it. One row
-- waits per Event, kind and team or person. Ids and codes only: a users row
-- deleted outright takes its person's rows; account erasure deletes them
-- and forgets the actor.
--
-- The release before this one runs on the new schema: it writes none of
-- the new rows and never reads the queue.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview',
    'co_organizer.add', 'co_organizer.remove', 'co_organizer.change',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile', 'user.roles',
    'keycloak.client', 'keycloak.role'));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_id_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_id_check CHECK (
    group_id IS NOT NULL
    OR (action = 'team.create' AND outcome <> 'ok')
    OR (action IN ('app.assign', 'app.unassign') AND target_user_id IS NOT NULL)
    OR (action LIKE 'user.%' AND target_user_id IS NOT NULL)
    OR action LIKE 'keycloak.%'
    OR action LIKE 'co_organizer.%');

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_group_path_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_group_path_check CHECK (
    (action NOT LIKE 'user.%' AND action NOT LIKE 'keycloak.%' AND group_path LIKE '/%' AND char_length(group_path) <= 1024
        AND (action NOT LIKE 'co_organizer.%' OR group_id IS NOT NULL))
    OR (action IN ('app.assign', 'app.unassign') AND group_path = '' AND group_id IS NULL)
    OR ((action LIKE 'user.%' OR action LIKE 'keycloak.%') AND group_path = '' AND group_id IS NULL)
    OR (action LIKE 'co_organizer.%' AND group_path = '' AND group_id IS NULL));

CREATE TABLE IF NOT EXISTS co_organizer_notifications (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('added', 'removed')),
    team_code TEXT,
    user_id UUID REFERENCES users (id) ON DELETE CASCADE,
    actor_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    claimed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    CONSTRAINT co_organizer_notifications_one_kind_check CHECK ((team_code IS NULL) <> (user_id IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS co_organizer_notifications_team_key
    ON co_organizer_notifications (event_id, kind, team_code) WHERE team_code IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS co_organizer_notifications_user_key
    ON co_organizer_notifications (event_id, kind, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS co_organizer_notifications_due_idx ON co_organizer_notifications (next_attempt_at);
CREATE INDEX IF NOT EXISTS co_organizer_notifications_user_idx ON co_organizer_notifications (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS co_organizer_notifications_actor_idx ON co_organizer_notifications (actor_id) WHERE actor_id IS NOT NULL;
