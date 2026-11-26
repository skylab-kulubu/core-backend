SET LOCAL lock_timeout = '5s';

-- Rolling back turns the switch off and takes back every link core made
-- (docs/guest-account-links.md, "Rolling back"): the release before does
-- not clear a linked guest Ticket's name and e-mail on account erasure, so
-- no linked Ticket may stay. A certificate issued from a linked Ticket after
-- the link took the Ticket's owner: it goes back too. Every other owner
-- stays. The columns and the switch stay (the up migration finds them); the
-- release before neither reads nor writes them. To remove them for good, by
-- hand: ALTER TABLE tickets DROP CONSTRAINT tickets_owner_link_check, DROP
-- COLUMN owner_linked_at, DROP COLUMN owner_link_source; the same on
-- certificates; DROP TABLE guest_account_link_settings;
UPDATE guest_account_link_settings SET enabled_at = NULL;
UPDATE certificates c SET owner_id = NULL
FROM tickets t
WHERE c.ticket_id = t.id AND t.owner_link_source IS NOT NULL
  AND c.owner_link_source IS NULL AND c.owner_id = t.owner_id;
UPDATE certificates SET owner_id = NULL, owner_linked_at = NULL, owner_link_source = NULL
WHERE owner_link_source IS NOT NULL;
UPDATE tickets SET owner_id = NULL, owner_linked_at = NULL, owner_link_source = NULL
WHERE owner_link_source IS NOT NULL;
