-- Rolling back drops the rows' way (invitation, access_request,
-- access_review); the rows themselves stay. The release before writes no
-- via, so rolling back the code alone needs no down migration: leave the
-- column. The finish-only trigger goes back to its list without via first.
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

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_via_check;
ALTER TABLE team_audit_records DROP COLUMN IF EXISTS via;
