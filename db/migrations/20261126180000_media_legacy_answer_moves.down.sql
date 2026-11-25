SET LOCAL lock_timeout = '5s';

-- Rolling back drops the move's record only. Moved Media stay private,
-- encrypted answer_file Media (their public objects are gone); their scan
-- deadline then runs from their upload again, so one still waiting for its
-- scan would be rejected as scan_timeout. Roll back only after every moved
-- Media has been scanned.
DROP TABLE IF EXISTS media_legacy_answer_moves;
