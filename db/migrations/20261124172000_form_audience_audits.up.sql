-- The audit record of the Form respondents audience (ADR-0075 karar 6,
-- review T4; docs/news-audience.md "Form respondents"): one row each time a
-- send or an approval resolves a form's respondents. Who sent, which form,
-- when, why and how many; never an address or a name. Kept on legitimate
-- interest: the sender is an officer of the club. Its period goes to the
-- ADR-0062 table before a retention rule deletes it.
--
-- The release before this one keeps working on the new schema: it reads and
-- writes none of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS form_audience_audits (
    id BIGSERIAL PRIMARY KEY,
    resolved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_sub UUID NOT NULL,
    form_id UUID NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('islem', 'duyuru')),
    category TEXT NOT NULL DEFAULT '',
    promotional BOOLEAN NOT NULL DEFAULT false,
    recipients INTEGER NOT NULL CHECK (recipients >= 0),
    client TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS form_audience_audits_resolved_idx ON form_audience_audits (resolved_at);
