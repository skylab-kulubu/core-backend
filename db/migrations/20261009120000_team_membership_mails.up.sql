-- Team membership mail (docs/team-membership-mail.md): a person added to a
-- team, or removed from one, through core's membership routes waits here
-- until SkyMail has taken the mail (internal/teammail). One row per change;
-- it is deleted once the mail is sent, skipped or refused, and given up after
-- 72 hours. A pass leases the rows it takes by moving next_attempt_at past
-- the lease, and a failed send moves it past its backoff.
--
-- Ids and the Group's path only: the address and the names are read from
-- Keycloak and core's row when the mail is sent, so nobody erased meanwhile
-- is written to. No foreign key to users: a person core has no row for yet
-- may be added to a team. Account erasure deletes the person's rows and
-- forgets them as the actor (anonymize_core).
CREATE TABLE IF NOT EXISTS team_membership_mails (
    id              UUID        PRIMARY KEY,
    subject_id      UUID        NOT NULL,
    actor_id        UUID,
    group_path      TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    CONSTRAINT team_membership_mails_action_check CHECK (action IN ('added', 'removed')),
    CONSTRAINT team_membership_mails_group_path_check CHECK (group_path LIKE '/%' AND char_length(group_path) <= 1024),
    CONSTRAINT team_membership_mails_attempts_check CHECK (attempts >= 0)
);

COMMENT ON TABLE team_membership_mails IS
    'Team membership mails waiting for SkyMail (docs/team-membership-mail.md). Ids and a Group path, no address or name; rows live until sent, at most 72 hours.';

CREATE INDEX IF NOT EXISTS team_membership_mails_due_idx ON team_membership_mails (next_attempt_at);
CREATE INDEX IF NOT EXISTS team_membership_mails_subject_idx ON team_membership_mails (subject_id);
CREATE INDEX IF NOT EXISTS team_membership_mails_actor_idx ON team_membership_mails (actor_id) WHERE actor_id IS NOT NULL;
