SET LOCAL lock_timeout = '5s';

-- A guest Ticket linked to the account its address is (docs/guest-account-links.md):
-- the Ticket stays GUEST with its name and e-mail, owner_id names the
-- account that holds the address verified (ADR-0044's addresses).
-- owner_linked_at and owner_link_source say that core linked it and how:
-- 'write' when the Ticket was written, 'sync' after the user sync's pass,
-- 'bulk' by the one-time command. A certificate without an owner is linked
-- the same way. Only rows with a source are unlinked when this is rolled
-- back; every other owner stays as it was.
--
-- guest_account_link_settings is the one switch: enabled_at NULL links
-- nothing anywhere. The migration leaves it off; the command's -apply links
-- the Tickets there are and turns it on.
--
-- The release before neither reads nor writes any of it.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS owner_linked_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS owner_link_source TEXT;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_owner_link_check;
-- NOT VALID: no scan under the ALTER's lock. The columns are new, so every
-- row before holds it; new and changed rows are checked.
ALTER TABLE tickets ADD CONSTRAINT tickets_owner_link_check CHECK (
    (owner_link_source IS NULL AND owner_linked_at IS NULL)
    OR (owner_link_source IN ('write', 'sync', 'bulk') AND owner_linked_at IS NOT NULL)) NOT VALID;

ALTER TABLE certificates ADD COLUMN IF NOT EXISTS owner_linked_at TIMESTAMPTZ;
ALTER TABLE certificates ADD COLUMN IF NOT EXISTS owner_link_source TEXT;
ALTER TABLE certificates DROP CONSTRAINT IF EXISTS certificates_owner_link_check;
ALTER TABLE certificates ADD CONSTRAINT certificates_owner_link_check CHECK (
    (owner_link_source IS NULL AND owner_linked_at IS NULL)
    OR (owner_link_source IN ('sync', 'bulk') AND owner_linked_at IS NOT NULL)) NOT VALID;

CREATE TABLE IF NOT EXISTS guest_account_link_settings (
    id BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    enabled_at TIMESTAMPTZ
);
INSERT INTO guest_account_link_settings (id) VALUES (true) ON CONFLICT (id) DO NOTHING;
