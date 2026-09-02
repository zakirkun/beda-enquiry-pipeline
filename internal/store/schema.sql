-- BEDA Enquiry Intelligence Pipeline — schema (see docs/02-ERD.md)
-- Applied idempotently on API boot. ponytail: no migration tool; add golang-migrate
-- when schema changes need ordered up/down instead of CREATE IF NOT EXISTS.

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS app_user (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        text NOT NULL,
    role        text NOT NULL,               -- sales_rep | support_agent | ops_admin | manager
    team        text NOT NULL
);

CREATE TABLE IF NOT EXISTS company (
    id       uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    name     text NOT NULL,
    domain   text,
    industry text
);
CREATE UNIQUE INDEX IF NOT EXISTS company_domain_key ON company (lower(domain)) WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS company_name_trgm ON company USING gin (name gin_trgm_ops);

CREATE TABLE IF NOT EXISTS enquiry (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_channel    text NOT NULL,          -- email | web_form | messaging
    received_at       timestamptz NOT NULL DEFAULT now(),
    sender_identifier text NOT NULL,
    subject           text,
    normalized_text   text NOT NULL,
    raw_payload       jsonb NOT NULL,         -- stands in for MinIO object; original never lost
    dedupe_hash       text NOT NULL,
    idempotency_key   text NOT NULL,
    status            text NOT NULL DEFAULT 'received',
    status_reason     text,
    attempts          int  NOT NULL DEFAULT 0,
    locked_until      timestamptz,
    next_attempt_at   timestamptz NOT NULL DEFAULT now()
);
-- Idempotency: a redelivered webhook can never create a second enquiry.
CREATE UNIQUE INDEX IF NOT EXISTS enquiry_idempotency_key ON enquiry (idempotency_key);
CREATE INDEX IF NOT EXISTS enquiry_dedupe_hash ON enquiry (dedupe_hash, received_at DESC);
CREATE INDEX IF NOT EXISTS enquiry_claim ON enquiry (status, next_attempt_at);

CREATE TABLE IF NOT EXISTS attachment (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    enquiry_id  uuid NOT NULL REFERENCES enquiry(id) ON DELETE CASCADE,
    storage_ref text NOT NULL,
    mime_type   text,
    size_bytes  bigint
);

CREATE TABLE IF NOT EXISTS extraction_result (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    enquiry_id          uuid NOT NULL REFERENCES enquiry(id) ON DELETE CASCADE,
    model_used          text NOT NULL,
    extracted_json      jsonb NOT NULL,
    raw_model_output    text,
    confidence_score    double precision NOT NULL,
    hallucination_flags jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now()
);
-- One authoritative extraction per enquiry; reprocessing overwrites rather than duplicating.
CREATE UNIQUE INDEX IF NOT EXISTS extraction_result_enquiry_key ON extraction_result (enquiry_id);

CREATE TABLE IF NOT EXISTS contact (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    name              text,
    email             text,
    phone             text,
    company_id        uuid REFERENCES company(id),
    source_enquiry_id uuid REFERENCES enquiry(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS contact_email_key ON contact (lower(email)) WHERE email IS NOT NULL;
CREATE INDEX IF NOT EXISTS contact_phone_idx ON contact (phone) WHERE phone IS NOT NULL;
CREATE INDEX IF NOT EXISTS contact_name_trgm ON contact USING gin (name gin_trgm_ops);

CREATE TABLE IF NOT EXISTS crm_record (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    type          text NOT NULL,             -- lead | opportunity | ticket
    contact_id    uuid NOT NULL REFERENCES contact(id),
    owner_user_id uuid REFERENCES app_user(id),
    team          text,
    status        text NOT NULL DEFAULT 'open',
    stage         text,
    enquiry_id    uuid REFERENCES enquiry(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- Idempotency: one CRM record per enquiry, so a retried worker cannot double-create.
CREATE UNIQUE INDEX IF NOT EXISTS crm_record_enquiry_key ON crm_record (enquiry_id) WHERE enquiry_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS message (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    crm_record_id uuid REFERENCES crm_record(id) ON DELETE CASCADE,
    enquiry_id    uuid NOT NULL REFERENCES enquiry(id) ON DELETE CASCADE,
    direction     text NOT NULL,             -- inbound | outbound
    kind          text NOT NULL,             -- reply | clarifying_question
    body          text NOT NULL,
    status        text NOT NULL,             -- draft | pending_approval | approved | sent | rejected
    drafted_by    text NOT NULL,             -- model id, or a user id when edited
    created_at    timestamptz NOT NULL DEFAULT now(),
    sent_at       timestamptz
);
-- Idempotency: one outbound draft per enquiry+kind, so retries cannot queue a second send.
CREATE UNIQUE INDEX IF NOT EXISTS message_enquiry_kind_key
    ON message (enquiry_id, kind) WHERE direction = 'outbound';

CREATE TABLE IF NOT EXISTS approval_action (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    message_id    uuid REFERENCES message(id) ON DELETE CASCADE,
    actor_user_id uuid NOT NULL REFERENCES app_user(id),
    decision      text NOT NULL,             -- approved | approved_with_edit | rejected
    notes         text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS audit_log_entry (
    id                    uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    enquiry_id            uuid REFERENCES enquiry(id) ON DELETE CASCADE,
    entity_type           text NOT NULL,
    entity_id             uuid,
    action                text NOT NULL,
    actor                 text NOT NULL,     -- 'system:<stage>' or 'user:<uuid>'
    payload_snapshot_ref  jsonb,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_enquiry_idx ON audit_log_entry (enquiry_id, created_at);

CREATE TABLE IF NOT EXISTS duplicate_match_candidate (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    enquiry_id        uuid NOT NULL REFERENCES enquiry(id) ON DELETE CASCADE,
    matched_contact_id uuid NOT NULL REFERENCES contact(id),
    match_score       double precision NOT NULL,
    match_method      text NOT NULL,         -- email_exact | phone_exact | trgm_name | trgm_company
    resolution        text NOT NULL DEFAULT 'pending', -- pending | auto_attached | attached | separate
    resolved_by       uuid REFERENCES app_user(id),
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS dup_candidate_key
    ON duplicate_match_candidate (enquiry_id, matched_contact_id);

-- Routing policy as data, editable by Ops without a deploy (docs/02-ERD.md).
CREATE TABLE IF NOT EXISTS routing_rule (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    name           text NOT NULL UNIQUE,
    condition_json jsonb NOT NULL,           -- {"enquiry_type":"sales","urgency":"high"}
    target_team    text NOT NULL,
    target_user_id uuid REFERENCES app_user(id),
    priority       int  NOT NULL DEFAULT 10,
    active         boolean NOT NULL DEFAULT true
);
