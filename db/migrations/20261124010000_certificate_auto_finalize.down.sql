-- Drops automatic attendance finalization. A rescan batch becomes a manual
-- one (both were asked for by a person, requested_by); which finalizations
-- were automatic, and every Event's switch, are lost.
UPDATE certificate_batches SET reason = 'manual' WHERE reason = 'rescan';
ALTER TABLE certificate_batches DROP CONSTRAINT IF EXISTS certificate_batches_reason_check;
ALTER TABLE certificate_batches ADD CONSTRAINT certificate_batches_reason_check
    CHECK (reason IN ('finalization', 'manual', 'reissue'));
ALTER TABLE certificate_event_state
    DROP CONSTRAINT IF EXISTS certificate_event_state_finalized_source_check,
    DROP COLUMN IF EXISTS finalized_source;
ALTER TABLE events DROP COLUMN IF EXISTS attendance_auto_finalize;
