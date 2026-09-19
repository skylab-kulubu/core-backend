ALTER TABLE certificates
    DROP CONSTRAINT IF EXISTS certificates_job_fk,
    DROP CONSTRAINT IF EXISTS certificates_batch_fk,
    DROP CONSTRAINT IF EXISTS certificates_template_version_fk;

DROP TABLE IF EXISTS certificate_jobs;
DROP TABLE IF EXISTS certificate_batches;
DROP TABLE IF EXISTS certificate_event_state;
DROP TABLE IF EXISTS certificate_template_bindings;
DROP TABLE IF EXISTS certificate_template_versions;
DROP TABLE IF EXISTS certificate_templates;

ALTER TABLE certificates
    DROP COLUMN IF EXISTS job_id,
    DROP COLUMN IF EXISTS batch_id,
    DROP COLUMN IF EXISTS pdf_sha256,
    DROP COLUMN IF EXISTS pdf_key,
    DROP COLUMN IF EXISTS template_source,
    DROP COLUMN IF EXISTS template_version_id;
