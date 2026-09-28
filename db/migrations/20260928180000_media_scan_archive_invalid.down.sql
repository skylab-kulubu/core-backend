-- archive_invalid has no meaning before the ZIP check: refuse while a Media
-- or a rejection record holds it rather than lose why it was rejected.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM media WHERE scan_result = 'archive_invalid')
        OR EXISTS (SELECT 1 FROM media_scan_rejections WHERE result = 'archive_invalid') THEN
        RAISE EXCEPTION 'media scan archive_invalid down: Media are rejected as archive_invalid; settle them first';
    END IF;
END;
$$;

-- 20260928140000's checks.
ALTER TABLE media
    DROP CONSTRAINT IF EXISTS media_scan_result_check,
    ADD CONSTRAINT media_scan_result_check CHECK (
        (scan_result IS NULL OR scan_result IN ('clean', 'infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity'))
        AND (status = 'rejected') = (COALESCE(scan_result, '') IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity'))
        AND (status <> 'scanning' OR scan_result IS NULL)
        AND scan_attempts >= 0
    );

ALTER TABLE media_scan_rejections
    DROP CONSTRAINT IF EXISTS media_scan_rejections_result_check,
    ADD CONSTRAINT media_scan_rejections_result_check CHECK (result IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity'));
