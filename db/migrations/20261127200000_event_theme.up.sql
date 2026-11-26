SET LOCAL lock_timeout = '5s';

-- An Event's theme and ticket colour (docs/event-theme.md).
--
--   * theme: the key of the look its PDF ticket, its participants' mails
--     and its ticket page take, from core's catalogue
--     (internal/ticket/theme); NULL is Kurumsal. Core writes only a key of
--     the catalogue (400 event_theme_unknown otherwise); the check is the
--     key's shape alone, the forms service's brand rule, so that a theme
--     taken out of the catalogue later leaves its rows valid (core reads an
--     unknown key as Kurumsal).
--   * ticket_accent_color: the colour the Event gives its ticket page, a
--     #RRGGBB in capitals; NULL for none.
--
-- Both are new and NULL on every row: the checks are NOT VALID (no scan of
-- events under the ALTER's lock), new and changed rows are checked. The
-- release before neither reads nor writes them.
ALTER TABLE events ADD COLUMN IF NOT EXISTS theme VARCHAR(32);
ALTER TABLE events ADD COLUMN IF NOT EXISTS ticket_accent_color VARCHAR(7);
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_theme_check;
ALTER TABLE events ADD CONSTRAINT events_theme_check
    CHECK (theme IS NULL OR theme ~ '^[a-z0-9-]{1,32}$') NOT VALID;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_ticket_accent_color_check;
ALTER TABLE events ADD CONSTRAINT events_ticket_accent_color_check
    CHECK (ticket_accent_color IS NULL OR ticket_accent_color ~ '^#[0-9A-F]{6}$') NOT VALID;
