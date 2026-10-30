-- Refuses while a notice is recorded: without the rows the notice would go
-- again to everyone who already had it.
DO $$
BEGIN
    IF to_regclass('public.team_page_notices') IS NOT NULL
       AND EXISTS (SELECT 1 FROM team_page_notices) THEN
        RAISE EXCEPTION 'refusing to drop team_page_notices while it records a notice';
    END IF;
END
$$;

DROP TABLE IF EXISTS team_page_notices;
