ALTER TABLE certificates
    ADD COLUMN template_version_id UUID,
    ADD COLUMN template_source TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN pdf_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN pdf_sha256 TEXT NOT NULL DEFAULT '',
    ADD COLUMN batch_id UUID,
    ADD COLUMN job_id UUID;

CREATE TABLE certificate_templates (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    owner_team TEXT NOT NULL DEFAULT '',
    source_kind TEXT NOT NULL DEFAULT 'upload'
        CHECK (source_kind IN ('upload', 'canva', 'figma', 'sky')),
    source_ref TEXT NOT NULL DEFAULT '',
    source_edit_url TEXT NOT NULL DEFAULT '',
    draft_layout JSONB NOT NULL DEFAULT '{}'::jsonb,
    system BOOLEAN NOT NULL DEFAULT false,
    archived_at TIMESTAMPTZ,
    created_by UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX certificate_templates_owner_team_idx
    ON certificate_templates (owner_team)
    WHERE archived_at IS NULL;

CREATE TABLE certificate_template_versions (
    id UUID PRIMARY KEY,
    template_id UUID NOT NULL REFERENCES certificate_templates (id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    layout JSONB NOT NULL,
    asset_manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    checksum TEXT NOT NULL,
    published_by UUID REFERENCES users (id) ON DELETE SET NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

CREATE TABLE certificate_template_bindings (
    id UUID PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('club', 'ownerTeam', 'event')),
    scope_key TEXT NOT NULL,
    template_id UUID NOT NULL REFERENCES certificate_templates (id) ON DELETE RESTRICT,
    updated_by UUID REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope, scope_key)
);

CREATE TABLE certificate_event_state (
    event_id UUID PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
    attendance_finalized_at TIMESTAMPTZ NOT NULL,
    attendance_finalized_by UUID REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE certificate_batches (
    id UUID PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    template_version_id UUID NOT NULL REFERENCES certificate_template_versions (id) ON DELETE RESTRICT,
    template_source TEXT NOT NULL CHECK (template_source IN ('event', 'ownerTeam', 'clubDefault')),
    reason TEXT NOT NULL CHECK (reason IN ('finalization', 'manual', 'reissue')),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'completed', 'partial', 'failed', 'cancelled')),
    requested_by UUID REFERENCES users (id) ON DELETE SET NULL,
    total_count INTEGER NOT NULL DEFAULT 0,
    queued_count INTEGER NOT NULL DEFAULT 0,
    issued_count INTEGER NOT NULL DEFAULT 0,
    failed_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

CREATE INDEX certificate_batches_event_created_idx
    ON certificate_batches (event_id, created_at DESC);

CREATE TABLE certificate_jobs (
    id UUID PRIMARY KEY,
    batch_id UUID NOT NULL REFERENCES certificate_batches (id) ON DELETE CASCADE,
    event_id UUID NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'issued', 'failed', 'cancelled')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '',
    certificate_id UUID REFERENCES certificates (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (batch_id, ticket_id)
);

CREATE INDEX certificate_jobs_claim_idx
    ON certificate_jobs (status, next_attempt_at, created_at);

ALTER TABLE certificates
    ADD CONSTRAINT certificates_template_version_fk
        FOREIGN KEY (template_version_id) REFERENCES certificate_template_versions (id) ON DELETE RESTRICT,
    ADD CONSTRAINT certificates_batch_fk
        FOREIGN KEY (batch_id) REFERENCES certificate_batches (id) ON DELETE SET NULL,
    ADD CONSTRAINT certificates_job_fk
        FOREIGN KEY (job_id) REFERENCES certificate_jobs (id) ON DELETE SET NULL;

INSERT INTO certificate_templates (
    id, name, owner_team, source_kind, source_ref, draft_layout, system
) VALUES (
    '7a1e0b9e-8b79-4f84-a6d2-2cf2bd4e8f01',
    'SKY LAB Varsayılan Sertifika',
    '',
    'sky',
    'system-default',
    '{
      "width":1123,
      "height":794,
      "orientation":"landscape",
      "backgroundColor":"#ffffff",
      "elements":[
        {"id":"brand","kind":"staticText","text":"SKY LAB","x":86,"y":84,"width":951,"height":42,"fontFamily":"Arial","fontSize":22,"fontWeight":700,"color":"#111111","align":"center","opacity":1},
        {"id":"label","kind":"staticText","text":"KATILIM SERTİFİKASI","x":86,"y":184,"width":951,"height":42,"fontFamily":"Arial","fontSize":22,"fontWeight":600,"color":"#5b21b6","align":"center","opacity":1},
        {"id":"recipient","kind":"recipientName","x":120,"y":284,"width":883,"height":86,"fontFamily":"Arial","fontSize":48,"fontWeight":700,"color":"#111111","align":"center","opacity":1,"fit":"shrink"},
        {"id":"event","kind":"eventName","x":150,"y":397,"width":823,"height":58,"fontFamily":"Arial","fontSize":28,"fontWeight":500,"color":"#262626","align":"center","opacity":1,"fit":"shrink"},
        {"id":"serial","kind":"serial","x":86,"y":673,"width":520,"height":28,"fontFamily":"Arial","fontSize":12,"fontWeight":400,"color":"#737373","align":"left","opacity":1},
        {"id":"verify","kind":"verificationQr","x":891,"y":610,"width":132,"height":132,"opacity":1}
      ]
    }'::jsonb,
    true
);

INSERT INTO certificate_template_versions (
    id, template_id, version, layout, checksum
)
SELECT
    '89c0bfe1-5be5-4ddb-8686-4865f51e3401',
    id,
    1,
    draft_layout,
    'system-default-v1'
FROM certificate_templates
WHERE id = '7a1e0b9e-8b79-4f84-a6d2-2cf2bd4e8f01';

INSERT INTO certificate_template_bindings (
    id, scope, scope_key, template_id
) VALUES (
    'a9c8b46a-bef0-4aac-9af5-1f8a10c32501',
    'club',
    'SKY_LAB',
    '7a1e0b9e-8b79-4f84-a6d2-2cf2bd4e8f01'
);
