-- The way a change came (docs/team-audit.md): a member add an accepted
-- Invitation made, one an approved Access request made, a removal an Access
-- review decided. NULL is a change made directly, as every row before this
-- migration was. Only these three values; the actor and the basis stay
-- those of the person who made or decided the change.
ALTER TABLE team_audit_records ADD COLUMN IF NOT EXISTS via TEXT;

ALTER TABLE team_audit_records DROP CONSTRAINT IF EXISTS team_audit_records_via_check;
ALTER TABLE team_audit_records ADD CONSTRAINT team_audit_records_via_check
    CHECK (via IS NULL OR via IN ('invitation', 'access_request', 'access_review'));

COMMENT ON COLUMN team_audit_records.via IS
    'How the change came: invitation, access_request or access_review; NULL for a direct change. A code, never personal data.';

-- A row's way is written with the row and never changes after.
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
