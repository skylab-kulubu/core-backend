-- The trigger function as 20261012140000 wrote it, then the columns.
CREATE OR REPLACE FUNCTION public.competition_ledger_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    changeable CONSTANT TEXT[] := ARRAY['revoked_at', 'revoked_by', 'revoke_reason', 'recorded_by'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'competition ledger rows are never deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_ledger_append_only';
    END IF;
    IF (to_jsonb(NEW) - changeable) IS DISTINCT FROM (to_jsonb(OLD) - changeable)
        OR (NEW.recorded_by IS DISTINCT FROM OLD.recorded_by AND NEW.recorded_by IS NOT NULL)
        OR (OLD.revoked_at IS NOT NULL AND (
            NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
            OR NEW.revoke_reason IS DISTINCT FROM OLD.revoke_reason
            OR (NEW.revoked_by IS DISTINCT FROM OLD.revoked_by AND NEW.revoked_by IS NOT NULL)))
    THEN
        RAISE EXCEPTION 'competition ledger rows are only revoked, once'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_ledger_append_only';
    END IF;
    RETURN NEW;
END
$$;

ALTER TABLE competition_submissions
    DROP CONSTRAINT IF EXISTS competition_submissions_revoked_basis_revoked,
    DROP COLUMN IF EXISTS revoked_basis,
    DROP COLUMN IF EXISTS actor_basis;
