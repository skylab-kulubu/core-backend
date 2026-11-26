SET LOCAL lock_timeout = '5s';

-- Puts back 20261126110000's basis check, without ticket.resend and
-- ticket.link.renew among event_editor's actions. It is added NOT VALID so
-- that the rows of a resend or link renewal by an event editor (a
-- Co-organizer's) already written stay (the record is kept three years).
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action IN ('membership.term.expired', 'member.remove'))
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview'))
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok')) NOT VALID;
