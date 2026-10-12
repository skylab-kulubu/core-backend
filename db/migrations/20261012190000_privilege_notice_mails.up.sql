-- Privilege notice (docs/privilege-change-mail.md; grup-yonetimi 05, Yusuf
-- 2026-10-12): a role mapping of any Group changed, or a person added to or
-- removed from a Privileged Group (AUTHZ_PRIVILEGED_ROOTS), through core,
-- tells ADMIN (and, for a membership, the root's own members) by mail.
--
-- A change is queued as one row without a recipient (recipient_id NULL).
-- The worker reads who gets it, then replaces that row with one row per
-- recipient, in one statement fenced by the claim (claimed_at), and sends
-- those like the membership mail's rows: leased, retried with backoff,
-- given up after 72 hours, deleted once sent, skipped or refused.
--
-- A table of its own, not team_membership_mails: a core that predates it
-- (a rollout, a rollback) takes every row of that table and would mail a
-- notice row's recipient as if they had been added to the Group.
--
-- Ids, a Group path and role names only; addresses and names are read when
-- the mail is sent. Account erasure deletes the rows that go to the person
-- and forgets them as the subject and the actor (anonymize_core).
CREATE TABLE IF NOT EXISTS privilege_notice_mails (
    id              UUID        PRIMARY KEY,
    change_id       UUID        NOT NULL,
    recipient_id    UUID,
    event           TEXT        NOT NULL,
    group_path      TEXT        NOT NULL,
    subject_id      UUID,
    actor_id        UUID,
    roles_added     TEXT[]      NOT NULL DEFAULT '{}',
    roles_removed   TEXT[]      NOT NULL DEFAULT '{}',
    member_count    INTEGER,
    occurred_at     TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    claimed_at      TIMESTAMPTZ,
    CONSTRAINT privilege_notice_mails_event_check CHECK (event IN ('member_added', 'member_removed', 'roles_changed')),
    CONSTRAINT privilege_notice_mails_group_path_check CHECK (group_path LIKE '/%' AND char_length(group_path) <= 1024),
    CONSTRAINT privilege_notice_mails_roles_check CHECK (cardinality(roles_added) <= 256 AND cardinality(roles_removed) <= 256),
    CONSTRAINT privilege_notice_mails_member_count_check CHECK (member_count IS NULL OR member_count >= 0),
    CONSTRAINT privilege_notice_mails_attempts_check CHECK (attempts >= 0)
);

COMMENT ON TABLE privilege_notice_mails IS
    'Privilege notices waiting for SkyMail (docs/privilege-change-mail.md). Ids, a Group path and role names, no address or name; rows live until sent, at most 72 hours.';

CREATE INDEX IF NOT EXISTS privilege_notice_mails_due_idx ON privilege_notice_mails (next_attempt_at);
-- One row per change before it is read for recipients, one per recipient after.
CREATE UNIQUE INDEX IF NOT EXISTS privilege_notice_mails_change_idx ON privilege_notice_mails (change_id) WHERE recipient_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS privilege_notice_mails_recipient_idx ON privilege_notice_mails (change_id, recipient_id) WHERE recipient_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS privilege_notice_mails_person_idx ON privilege_notice_mails (recipient_id) WHERE recipient_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS privilege_notice_mails_subject_idx ON privilege_notice_mails (subject_id) WHERE subject_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS privilege_notice_mails_actor_idx ON privilege_notice_mails (actor_id) WHERE actor_id IS NOT NULL;
