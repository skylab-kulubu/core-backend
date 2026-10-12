-- Retired Team codes (docs/team-codes.md): a Group's Keycloak name is its
-- Team code, the key of Owner team records, the CMS's Teams and Forms'
-- templates. A code given up by a rename is kept here for good, so that no
-- new Group takes it and with it the old team's records. Codes and Group ids
-- only, no person; kept without a time limit.
CREATE TABLE IF NOT EXISTS retired_team_codes (
    code       TEXT        PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    group_id   UUID,
    reason     TEXT        NOT NULL,
    CONSTRAINT retired_team_codes_code_check CHECK (code <> '' AND char_length(code) <= 255),
    CONSTRAINT retired_team_codes_reason_check CHECK (reason <> '')
);

COMMENT ON TABLE retired_team_codes IS
    'Team codes no new Group may take (docs/team-codes.md). Codes and Group ids, no person; kept without a time limit.';
