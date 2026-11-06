-- What a ledger row's writer, and its revoker, wrote on (docs/competitions.md
-- "Who", yarışma modülü ticket 25): Privileged, a Leader, a member while the
-- Team's setting lets all members manage, or later a Competition's staff.
-- After the setting is turned off a member's rows still say why they were
-- allowed. Rows written before stay NULL: their basis was not kept.
ALTER TABLE competition_submissions
    ADD COLUMN IF NOT EXISTS actor_basis TEXT
        CONSTRAINT competition_submissions_actor_basis_check
        CHECK (actor_basis IN ('privileged', 'leader', 'member_setting', 'staff')),
    ADD COLUMN IF NOT EXISTS revoked_basis TEXT
        CONSTRAINT competition_submissions_revoked_basis_check
        CHECK (revoked_basis IN ('privileged', 'leader', 'member_setting', 'staff'));

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.competition_submissions'::regclass
          AND conname = 'competition_submissions_revoked_basis_revoked'
    ) THEN
        ALTER TABLE competition_submissions
            ADD CONSTRAINT competition_submissions_revoked_basis_revoked
            CHECK (revoked_at IS NOT NULL OR revoked_basis IS NULL);
    END IF;
END
$$;

-- The ledger stays append-only: revoked_basis is written with the
-- revocation, once, like the other revocation columns; actor_basis never
-- changes. Adjustments have neither column: subtracting a key they lack
-- changes nothing.
CREATE OR REPLACE FUNCTION public.competition_ledger_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    changeable CONSTANT TEXT[] := ARRAY['revoked_at', 'revoked_by', 'revoke_reason', 'revoked_basis', 'recorded_by'];
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
            OR (to_jsonb(NEW) -> 'revoked_basis') IS DISTINCT FROM (to_jsonb(OLD) -> 'revoked_basis')
            OR (NEW.revoked_by IS DISTINCT FROM OLD.revoked_by AND NEW.revoked_by IS NOT NULL)))
    THEN
        RAISE EXCEPTION 'competition ledger rows are only revoked, once'
            USING ERRCODE = '23514', CONSTRAINT = 'competition_ledger_append_only';
    END IF;
    RETURN NEW;
END
$$;
