SET LOCAL lock_timeout = '5s';

-- An Event's raffles (docs/raffle.md). event_raffles keeps each draw:
-- its title, its criteria, the eligible Tickets (ids, in their list's
-- order), the list's SHA-256, the seed (crypto/rand) and the algorithm,
-- so that anyone given them can draw the same order again; who drew it
-- and when. event_raffle_entries keeps its places in the draw's order:
-- winners, then alternates; a winner who did not come is forfeited (who,
-- when, the reason) and the first alternate left becomes a winner. Ticket
-- ids only, no name nor address; a Ticket keeps its row (anonymous) when
-- its holder's account is erased, so the record stays verifiable. Who drew
-- or forfeited is forgotten on erasure (no foreign key: the actor is not
-- always a users row), as is a reason about an erased holder's Ticket.
--
-- The Team audit record takes raffle.draw and raffle.forfeit, on the bases
-- a Ticket operator's rows take (privileged, leader, event_editor). The
-- checks keep every action and basis of 20261127220000 (ticket.walk_in,
-- door_staff) and 20261127223000 (ticket.export), whichever of them ran,
-- so they hold in any order those releases go out.
--
-- The release before runs on the new schema: it neither reads nor writes
-- the new tables and writes none of the new rows.
CREATE TABLE IF NOT EXISTS event_raffles (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    criteria JSONB NOT NULL,
    eligible_ticket_ids UUID[] NOT NULL,
    eligible_count INT NOT NULL,
    winner_count INT NOT NULL,
    alternate_count INT NOT NULL,
    list_hash TEXT NOT NULL,
    seed TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    drawn_by UUID,
    drawn_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT event_raffles_title_check CHECK (char_length(title) BETWEEN 1 AND 120),
    CONSTRAINT event_raffles_counts_check CHECK (
        eligible_count = cardinality(eligible_ticket_ids)
        AND winner_count BETWEEN 1 AND 100 AND alternate_count BETWEEN 0 AND 100
        AND winner_count + alternate_count <= eligible_count),
    CONSTRAINT event_raffles_list_hash_check CHECK (list_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT event_raffles_seed_check CHECK (seed ~ '^[0-9a-f]{64}$'),
    CONSTRAINT event_raffles_algorithm_check CHECK (algorithm IN ('sky-raffle-v1'))
);
CREATE INDEX IF NOT EXISTS event_raffles_event_idx ON event_raffles (event_id, drawn_at DESC);

CREATE TABLE IF NOT EXISTS event_raffle_entries (
    raffle_id UUID NOT NULL REFERENCES event_raffles (id) ON DELETE CASCADE,
    position INT NOT NULL,
    ticket_id UUID NOT NULL,
    status TEXT NOT NULL,
    promoted_at TIMESTAMPTZ,
    promoted_by UUID,
    forfeited_at TIMESTAMPTZ,
    forfeited_by UUID,
    forfeit_reason TEXT,
    PRIMARY KEY (raffle_id, position),
    CONSTRAINT event_raffle_entries_ticket_key UNIQUE (raffle_id, ticket_id),
    CONSTRAINT event_raffle_entries_position_check CHECK (position >= 1),
    CONSTRAINT event_raffle_entries_status_check CHECK (status IN ('winner', 'alternate', 'forfeited')),
    CONSTRAINT event_raffle_entries_forfeit_check CHECK (
        (status = 'forfeited') = (forfeited_at IS NOT NULL)
        AND (forfeit_reason IS NULL OR char_length(forfeit_reason) BETWEEN 3 AND 300))
);
CREATE INDEX IF NOT EXISTS event_raffle_entries_ticket_idx ON event_raffle_entries (ticket_id);

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore', 'ticket.promote',
    'ticket.import', 'ticket.import.preview', 'ticket.walk_in', 'ticket.export',
    'raffle.draw', 'raffle.forfeit',
    'co_organizer.add', 'co_organizer.remove', 'co_organizer.change',
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
        AND action IN ('membership.term.expired', 'member.remove'))
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview',
            'ticket.export', 'ticket.resend', 'ticket.link.renew', 'co_organizer.add',
            'raffle.draw', 'raffle.forfeit'))
    OR (actor_basis = 'door_staff' AND actor_via_group_id IS NOT NULL AND action = 'ticket.walk_in')
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));
