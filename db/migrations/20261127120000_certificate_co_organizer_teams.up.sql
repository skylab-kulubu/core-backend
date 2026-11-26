SET LOCAL lock_timeout = '5s';

-- A certificate's snapshot of its Event's Co-organizer teams shown outside
-- the panel when it was issued (docs/co-organizers.md), as owner_team is of
-- its Owner team: the verification page names them, the PDF does not, and
-- a later change of the list or of a team's visibility leaves it as it
-- was. NULL is a certificate issued before core kept it (no line shown);
-- an empty array is "none".
--
-- The release before this one runs on the new schema: its inserts name no
-- such column (NULL) and it never reads it.
ALTER TABLE certificates ADD COLUMN IF NOT EXISTS co_organizer_teams TEXT[];
COMMENT ON COLUMN certificates.co_organizer_teams IS
    'The Event''s co_organizer teams when the certificate was issued (docs/co-organizers.md); NULL before core kept it.';
