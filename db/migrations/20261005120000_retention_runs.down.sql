-- The records are the proof of destruction (KVKK deletion regulation art.
-- 7(3)): once a run is recorded, this migration is forward-only.
LOCK TABLE retention_periods, retention_runs, retention_run_rules IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM retention_runs) OR EXISTS (SELECT 1 FROM retention_periods) THEN
        RAISE EXCEPTION 'retention records exist and are kept at least three years: this migration is forward-only';
    END IF;
END
$$;

DROP TABLE IF EXISTS retention_run_rules;
DROP TABLE IF EXISTS retention_runs;
DROP TABLE IF EXISTS retention_periods;
