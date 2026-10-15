-- SkyPass in Google Wallet (docs/skypass-google-wallet.md): one row per pass
-- core issued. A pass is a Google Wallet object whose rotating barcode shows
-- SPW1:<pass_id>:<TOTP>; the door's check-in reads the code back here.
--
-- No secret is kept: each pass's TOTP secret is derived from the Wallet key
-- (SKYPASS_GOOGLE_WALLET_TOTP_KEY) and pass_id, which every barcode shows.
-- No name or skyNumber either: Google holds those on the object, and core
-- writes them from users each time it issues the link.
--
--   * last_counter: the highest TOTP step a check-in accepted. A code of that
--     step or an older one checks no one in again (one use per code).
--   * revoked_at: the pass no longer opens the door (the person revoked it,
--     or erasure began); the row stays only until Google has withdrawn its
--     copy, then it is deleted.
--
-- At most one pass of a person is not revoked. Account erasure withdraws and
-- deletes every row of the person (step erase_skypass_wallet).
CREATE TABLE IF NOT EXISTS skypass_google_wallet_passes (
    pass_id TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    last_counter BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    CONSTRAINT skypass_google_wallet_passes_pass_id_check CHECK (pass_id ~ '^[A-Z2-7]{26}$'),
    CONSTRAINT skypass_google_wallet_passes_last_counter_check CHECK (last_counter >= 0)
);

COMMENT ON TABLE skypass_google_wallet_passes IS
    'SkyPass Google Wallet passes (docs/skypass-google-wallet.md). No secret, no name; account erasure withdraws and deletes a person''s rows.';

CREATE UNIQUE INDEX IF NOT EXISTS skypass_google_wallet_passes_active_user_idx
    ON skypass_google_wallet_passes (user_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS skypass_google_wallet_passes_user_idx
    ON skypass_google_wallet_passes (user_id);

-- user_id is a current-identity link like any other (account-lifecycle.md):
-- only an active subject with no deletion marker may be named.
DROP TRIGGER IF EXISTS skypass_google_wallet_passes_require_active_subject ON skypass_google_wallet_passes;
CREATE TRIGGER skypass_google_wallet_passes_require_active_subject
    BEFORE INSERT OR UPDATE OF user_id ON skypass_google_wallet_passes
    FOR EACH ROW EXECUTE FUNCTION public.require_active_account_reference('user_id');
