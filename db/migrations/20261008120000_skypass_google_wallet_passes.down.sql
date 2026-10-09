-- Rolling back drops the only record of which Google Wallet objects hold a
-- person's name and skyNumber, and so the means to withdraw them. Refuse
-- while any row exists; revoke the passes (or ship a forward repair) instead.
LOCK TABLE skypass_google_wallet_passes IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM skypass_google_wallet_passes) THEN
        RAISE EXCEPTION 'skypass_google_wallet_passes holds issued passes; refusing to drop it';
    END IF;
END
$$;

DROP TABLE IF EXISTS skypass_google_wallet_passes;
