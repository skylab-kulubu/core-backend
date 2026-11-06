-- Drops the index and puts the moved rows back. Only the index goes: the
-- codes stay retired (the table's own down, which forgets them, is never run
-- in production).
DROP INDEX IF EXISTS retired_team_codes_lower_code_idx;

INSERT INTO retired_team_codes (code, retired_at, group_id, reason)
SELECT code, retired_at, group_id, reason FROM retired_team_codes_case_duplicates
ON CONFLICT (code) DO NOTHING;

DROP TABLE IF EXISTS retired_team_codes_case_duplicates;
