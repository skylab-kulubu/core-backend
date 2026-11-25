SET LOCAL lock_timeout = '5s';

-- The offline door (docs/offline-door.md): a scan the door app took while
-- offline and sent later carries the moment the device took it. Core keeps
-- it next to created_at (when core wrote the row). NULL is a scan core took
-- as it came, as every row before this migration. Nullable and without a
-- default: adding the column rewrites nothing.
ALTER TABLE ticket_checkins ADD COLUMN IF NOT EXISTS scanned_at TIMESTAMPTZ;
ALTER TABLE venue_entries ADD COLUMN IF NOT EXISTS scanned_at TIMESTAMPTZ;

COMMENT ON COLUMN ticket_checkins.scanned_at IS
    'When the door device took the scan, for a scan sent later (offline); NULL when core took it as it came.';
COMMENT ON COLUMN venue_entries.scanned_at IS
    'When the door device took the scan, for a scan sent later (offline); NULL when core took it as it came.';
