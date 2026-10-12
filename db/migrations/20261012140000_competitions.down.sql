-- Rolling back drops the competitions and their ledger. It refuses while a
-- competition exists: the ledger is the only record of those scores.
DO $$
BEGIN
    IF to_regclass('public.competitions') IS NOT NULL AND EXISTS (SELECT 1 FROM competitions) THEN
        RAISE EXCEPTION 'competitions exist; export them before rolling back 20261012140000';
    END IF;
END
$$;

DROP TABLE IF EXISTS competition_adjustments;
DROP TABLE IF EXISTS competition_submissions;
DROP TABLE IF EXISTS competition_entrant_members;
DROP TABLE IF EXISTS competition_entrants;
DROP TABLE IF EXISTS competition_stages;
DROP TABLE IF EXISTS competitions;
DROP FUNCTION IF EXISTS public.competition_ledger_append_only();
DROP FUNCTION IF EXISTS public.competition_entrant_member_same_event();
