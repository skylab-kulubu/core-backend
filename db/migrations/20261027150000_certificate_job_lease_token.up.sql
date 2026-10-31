-- Names the claim holding a certificate job; only that claim may renew,
-- complete or fail it (horizontal-scale ticket 25). Expand only: an image
-- without it ignores the column.
SET LOCAL lock_timeout = '5s';
ALTER TABLE certificate_jobs ADD COLUMN IF NOT EXISTS lease_token UUID;
