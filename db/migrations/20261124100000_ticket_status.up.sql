-- A Ticket's status (kontenjan-iptal-hatirlatma 02; docs/ticket-cancellation.md):
-- active (the Ticket as before), waitlisted (on the Event's waiting list)
-- or cancelled (by its holder, an operator or the form that wrote it). A
-- Ticket is never deleted for this: every reader that means "has a Ticket"
-- reads active ones only (the door, the mails, certificates, competitions,
-- the counts and lists); erasure and retention see every status.
--
-- Every Ticket already there is active (the column's default). The release
-- before writes no status, so its inserts are active and it runs on this
-- schema unchanged.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS status_changed_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS waitlisted_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS cancelled_by TEXT;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS cancel_reason TEXT;

ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_status_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_status_check
    CHECK (status IN ('active', 'waitlisted', 'cancelled'));
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancelled_by_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_cancelled_by_check
    CHECK (cancelled_by IS NULL OR cancelled_by IN ('holder', 'operator', 'form'));
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancel_reason_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_cancel_reason_check
    CHECK (cancel_reason IS NULL OR char_length(cancel_reason) <= 500);
-- A cancelled Ticket says when and by whom; any other has neither.
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_cancelled_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_cancelled_check CHECK (
    (status = 'cancelled' AND cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL)
    OR (status <> 'cancelled' AND cancelled_at IS NULL AND cancelled_by IS NULL AND cancel_reason IS NULL));

COMMENT ON COLUMN tickets.status IS 'active, waitlisted or cancelled; only an active Ticket opens the door and is counted.';
COMMENT ON COLUMN tickets.cancel_reason IS 'The operator''s optional reason (at most 500 characters), shown to the holder.';

-- The Event's Tickets of one status: the panel's list and the counts.
CREATE INDEX IF NOT EXISTS tickets_event_status_idx ON tickets (event_id, status);

-- An operator cancelling a Ticket and taking the cancellation back are
-- recorded in the Team audit record (ticket.cancel, ticket.restore), as
-- ticket.resend: against the Event's team, after naming the Ticket and the
-- Event, never a person.
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_action_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_action_check CHECK (action IN (
    'member.add', 'member.remove', 'leader.add', 'leader.remove',
    'team.create', 'team.code', 'team.profile', 'team.move', 'team.delete',
    'group.roles', 'group.attributes', 'app.assign', 'app.unassign',
    'membership.term.extend', 'membership.term.handover', 'membership.term.expired',
    'ticket.resend', 'ticket.link.renew', 'ticket.cancel', 'ticket.restore',
    'user.enable', 'user.disable', 'user.password.set', 'user.password.mail',
    'user.email.change', 'user.email.verify', 'user.email.mail', 'user.email.primary',
    'user.required_actions', 'user.credential.remove', 'user.idp.unlink',
    'user.lockout.clear', 'user.profile'));
