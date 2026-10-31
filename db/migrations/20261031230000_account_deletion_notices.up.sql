-- The two account deletion notices (docs/account-lifecycle.md, "Deletion
-- notices"): when the worker took each in hand. A notice is claimed here,
-- under the lease, before it is sent, so a request never sends one twice;
-- a claim whose mail then failed is not tried again (the notices are best
-- effort, the erasure never waits for them). Times only: the addresses are
-- read for the pass and kept nowhere.
--
-- Nullable columns only: the release before this one never reads them.
SET LOCAL lock_timeout = '5s';

ALTER TABLE account_deletion_requests
    ADD COLUMN IF NOT EXISTS received_notice_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS completed_notice_at TIMESTAMPTZ;
