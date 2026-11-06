-- One retired code per code whatever its case (docs/team-codes.md,
-- ekip-yetki-devri 15). The primary key compares codes byte by byte, so
-- WEBLAB and weblab could both be written; core reads them without case
-- (teamcode.Held). The unique index on lower(code) makes the database say the
-- same, and is the index Held's lookup uses.
--
-- A database that already holds a code in two cases does not stop core's
-- start, and loses no row: the earliest row of each code stays (retired_at,
-- then code), the others move to retired_team_codes_case_duplicates with
-- the time they moved. The code stays retired either way.
--
-- A plain CREATE INDEX, in the file's one transaction with the move: the
-- table holds tens of codes and is written only by Group renames and
-- deletions, which wait the moment it takes (CREATE INDEX CONCURRENTLY
-- cannot share a transaction). SHARE ROW EXCLUSIVE keeps a retirement out
-- between the move and the build; reads go on.
LOCK TABLE retired_team_codes IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS retired_team_codes_case_duplicates (
    code       TEXT        PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL,
    group_id   UUID,
    reason     TEXT        NOT NULL,
    moved_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE retired_team_codes_case_duplicates IS
    'Rows of retired_team_codes whose code another row already held in another case, moved when the lower(code) index was made (docs/team-codes.md). Codes and Group ids, no person.';

WITH ranked AS (
    SELECT code, row_number() OVER (PARTITION BY lower(code) ORDER BY retired_at, code) AS n
    FROM retired_team_codes
), moved AS (
    DELETE FROM retired_team_codes r
    USING ranked
    WHERE ranked.code = r.code AND ranked.n > 1
    RETURNING r.code, r.retired_at, r.group_id, r.reason
)
INSERT INTO retired_team_codes_case_duplicates (code, retired_at, group_id, reason)
SELECT code, retired_at, group_id, reason FROM moved
ON CONFLICT (code) DO NOTHING;

CREATE UNIQUE INDEX IF NOT EXISTS retired_team_codes_lower_code_idx ON retired_team_codes (lower(code));
