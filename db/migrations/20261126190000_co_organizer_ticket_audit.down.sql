SET LOCAL lock_timeout = '5s';

-- Rolling back narrows event_editor to 20261126110000's actions: the rows
-- of a resend or link renewal by an event editor (a Co-organizer's) cannot
-- stay under it and go, as the console rows go in that migration's down.
-- The finish-only trigger forbids no DELETE (the retention sweep deletes).
DELETE FROM team_audit_records
WHERE actor_basis = 'event_editor' AND action IN ('ticket.resend', 'ticket.link.renew');

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
    OR (actor_basis = 'console' AND actor_via_group_id IS NULL AND via = 'console' AND outcome = 'ok'));
