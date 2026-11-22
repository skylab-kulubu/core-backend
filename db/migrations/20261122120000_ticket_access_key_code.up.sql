-- A Ticket's access key, code and QR version (ADR 0082; bilet-qr-ve-pdf
-- ticket 01; docs/ticket-access-key-and-code.md).
--
-- access_key opens the ticket page without a sign-in: 26 base32 characters
-- (A–Z, 2–7) of at least 128 random bits. Account erasure and guest
-- erasure empty it (NULL) and raise qr_version, so the erased person's
-- page and Ticket QR stop working; a Ticket already erased gets none here.
-- code is what the desk reads out:
-- 8 characters of Crockford's base32 (no I, L, O, U), kept without the
-- hyphen it is shown with. qr_version goes up when the panel renews the
-- link; a Ticket QR of an older version is refused online.
--
-- Core writes both values itself (internal/ticket/code.go, crypto/rand) and
-- tries again when a code is taken. The column defaults draw them in the
-- database instead, from gen_random_uuid() (a strong random source, in core
-- since PostgreSQL 13; no pgcrypto): they fill the Tickets already there
-- (a volatile default is computed for every row), and cover a Ticket the
-- release before this one writes during the deploy, which knows none of
-- the three columns and keeps working on the new schema. A code the
-- default draws twice is drawn again below, before the unique index.
SET LOCAL lock_timeout = '5s';

-- ticket_random_text is len characters of alphabet (32 of them), each from
-- one byte of a SHA-256 over two random UUIDs (244 random bits). 256 is a
-- multiple of 32, so every character is equally likely.
CREATE OR REPLACE FUNCTION ticket_random_text(alphabet TEXT, len INTEGER)
RETURNS TEXT
LANGUAGE sql
VOLATILE
AS $$
    SELECT string_agg(substr(alphabet, get_byte(r.b, i) % 32 + 1, 1), '' ORDER BY i)
    FROM (SELECT sha256(uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid())) AS b) r,
         generate_series(0, len - 1) AS i
$$;

ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS access_key TEXT
        DEFAULT ticket_random_text('ABCDEFGHIJKLMNOPQRSTUVWXYZ234567', 26)
        CONSTRAINT tickets_access_key_shape CHECK (access_key IS NULL OR access_key ~ '^[A-Z2-7]{26}$'),
    ADD COLUMN IF NOT EXISTS code TEXT NOT NULL
        DEFAULT ticket_random_text('0123456789ABCDEFGHJKMNPQRSTVWXYZ', 8)
        CONSTRAINT tickets_code_shape CHECK (code ~ '^[0-9A-HJKMNP-TV-Z]{8}$'),
    ADD COLUMN IF NOT EXISTS qr_version INTEGER NOT NULL DEFAULT 1
        CONSTRAINT tickets_qr_version_positive CHECK (qr_version >= 1);

-- A Ticket whose holder was erased before this migration (its member
-- detached, its guest's address emptied) has no page to open.
UPDATE tickets SET access_key = NULL
WHERE access_key IS NOT NULL
  AND ((ticket_type = 'REGISTERED' AND owner_id IS NULL) OR (ticket_type = 'GUEST' AND btrim(guest_email) = ''));

-- Two Tickets that drew one value: the later one draws again, until none do.
DO $$
BEGIN
    LOOP
        UPDATE tickets t
        SET code = ticket_random_text('0123456789ABCDEFGHJKMNPQRSTVWXYZ', 8)
        FROM (
            SELECT id, row_number() OVER (PARTITION BY code ORDER BY created_at, id) AS n FROM tickets
        ) d
        WHERE t.id = d.id AND d.n > 1;
        EXIT WHEN NOT FOUND;
    END LOOP;
    LOOP
        UPDATE tickets t
        SET access_key = ticket_random_text('ABCDEFGHIJKLMNOPQRSTUVWXYZ234567', 26)
        FROM (
            SELECT id, row_number() OVER (PARTITION BY access_key ORDER BY created_at, id) AS n
            FROM tickets WHERE access_key IS NOT NULL
        ) d
        WHERE t.id = d.id AND d.n > 1;
        EXIT WHEN NOT FOUND;
    END LOOP;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS tickets_access_key_idx ON tickets (access_key);
CREATE UNIQUE INDEX IF NOT EXISTS tickets_code_idx ON tickets (code);
