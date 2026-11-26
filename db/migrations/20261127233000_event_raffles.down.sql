SET LOCAL lock_timeout = '5s';

-- Rolling back keeps event_raffles and event_raffle_entries with their
-- rows: the raffles drawn are a record (docs/raffle.md), and the release
-- before neither reads nor writes them. The next release finds them again
-- (CREATE TABLE IF NOT EXISTS). The Team audit checks stay wide, as the
-- earlier migrations' downs leave them: raffle.* rows already written
-- stay valid. To remove the raffles for good, by hand: DROP TABLE
-- event_raffle_entries, event_raffles;
SELECT 1;
