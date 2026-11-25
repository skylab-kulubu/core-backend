-- Rolling back drops the console audit: the cursor, the notice marker, and
-- the console rows with their event ids (the via and basis checks narrow
-- again, so those rows cannot stay). The action check stays wide, as the
-- earlier migrations' downs leave it: no row of the new actions outlives
-- the console rows, which are the only ones that write them. The basis check
-- goes back to 20261126101700's (the term job's member.remove stays).
SET LOCAL lock_timeout = '5s';

ALTER TABLE privilege_notice_mails DROP CONSTRAINT IF EXISTS privilege_notice_mails_via_check;
ALTER TABLE privilege_notice_mails DROP COLUMN IF EXISTS via;

DROP TABLE IF EXISTS console_audit_cursor;

-- The finish-only trigger forbids no DELETE (the retention sweep deletes).
DELETE FROM team_audit_records WHERE via = 'console';

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_actor_basis_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_actor_basis_check CHECK (
    (actor_basis = 'privileged' AND actor_via_group_id IS NULL)
    OR (actor_basis = 'leader' AND actor_via_group_id IS NOT NULL)
    OR (actor_basis = 'none' AND actor_via_group_id IS NULL AND outcome = 'denied')
    OR (actor_basis = 'system' AND actor_via_group_id IS NULL
        AND actor_id = '00000000-0000-0000-0000-000000000000'
        AND action IN ('membership.term.expired', 'member.remove'))
    OR (actor_basis = 'event_editor' AND actor_via_group_id IS NOT NULL
        AND action IN ('ticket.cancel', 'ticket.restore', 'ticket.promote', 'ticket.import', 'ticket.import.preview')));

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_via_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_via_check
    CHECK (via IS NULL OR via IN ('invitation', 'access_request', 'access_review'));

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
        OR NEW.via IS DISTINCT FROM OLD.via THEN
        RAISE EXCEPTION 'team_audit_records: a row only changes from pending to its outcome'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP INDEX IF EXISTS team_audit_records_keycloak_event_idx;
ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_keycloak_event_id_check;
ALTER TABLE team_audit_records DROP COLUMN IF EXISTS keycloak_event_id;
