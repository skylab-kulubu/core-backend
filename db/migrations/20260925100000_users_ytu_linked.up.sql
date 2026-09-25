-- True once core has seen the YTÜ Microsoft login's university for the
-- person: university, faculty and department then follow that login.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS ytu_linked BOOLEAN NOT NULL DEFAULT false;
