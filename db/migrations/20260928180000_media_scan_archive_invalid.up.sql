-- The ZIP check (media redesign ticket 23): before a ZIP is scanned, the
-- scan worker reads it and rejects it when clamd could not scan all of it.
-- One that holds more than clamd scans whole is too_large_to_scan, as
-- before; one that is malformed, or holds what clamd cannot read (an
-- encrypted member, a compression method other than stored or deflate,
-- ZIP64 sizes, headers that disagree with what the members hold), is
-- archive_invalid: a new result, in both checks.
ALTER TABLE media
    DROP CONSTRAINT IF EXISTS media_scan_result_check,
    ADD CONSTRAINT media_scan_result_check CHECK (
        (scan_result IS NULL OR scan_result IN ('clean', 'infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity', 'archive_invalid'))
        AND (status = 'rejected') = (COALESCE(scan_result, '') IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity', 'archive_invalid'))
        AND (status <> 'scanning' OR scan_result IS NULL)
        AND scan_attempts >= 0
    );

ALTER TABLE media_scan_rejections
    DROP CONSTRAINT IF EXISTS media_scan_rejections_result_check,
    ADD CONSTRAINT media_scan_rejections_result_check CHECK (result IN ('infected', 'too_large_to_scan', 'lost', 'scan_timeout', 'integrity', 'archive_invalid'));
