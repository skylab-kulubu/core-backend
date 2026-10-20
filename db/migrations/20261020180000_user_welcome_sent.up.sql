-- When the Welcome mail went out to the person (docs/user-sync.md). The user
-- sync sends it once: to a row whose mark is empty and whose e-mail is set,
-- for an account Keycloak created on or after WELCOME_MAIL_SINCE, and marks
-- the row in the same step. Which path opened the row (a first request, the
-- admin list, the sync itself) no longer decides whether the mail goes.
--
-- Rows that exist now count as welcomed when they carry an e-mail: until now
-- the Welcome mail went out when the row was opened by a first request with
-- an e-mail claim. A row without an e-mail cannot have received one and
-- stays unmarked, so the sync sends it once Keycloak gives the address (and
-- the account is recent enough).
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS welcome_sent_at TIMESTAMPTZ;

UPDATE users
   SET welcome_sent_at = created_at
 WHERE welcome_sent_at IS NULL
   AND email <> '';
