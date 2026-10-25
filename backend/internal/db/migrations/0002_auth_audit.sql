-- 0002: kullanıcılar, oturumlar, denetim kaydı, idempotency anahtarları
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE users (
    id            bigserial PRIMARY KEY,
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    full_name     text        NOT NULL,
    role          text        NOT NULL CHECK (role IN ('subscriber','field_crew','billing','operator','analyst','admin')),
    subscriber_id bigint,      -- yalnızca role='subscriber' (FK 0003'te eklenir)
    crew_id       bigint,      -- yalnızca role='field_crew' (FK 0006'da eklenir)
    disabled      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_lower CHECK (email = lower(email) AND email LIKE '%@%'),
    CONSTRAINT users_subscriber_link CHECK ((role = 'subscriber') = (subscriber_id IS NOT NULL)),
    CONSTRAINT users_crew_link CHECK (role = 'field_crew' OR crew_id IS NULL)
);
CREATE UNIQUE INDEX users_email_uq ON users (email);

-- Refresh token'lar: yalnızca SHA-256 özeti saklanır. Her yenilemede eski token
-- iptal edilir (rotation); iptal edilmiş bir token tekrar kullanılırsa aynı
-- ailedeki tüm token'lar iptal edilir (reuse detection).
CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id   uuid        NOT NULL,
    token_hash  bytea       NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid REFERENCES refresh_tokens(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

-- Eklemeli-yalnız denetim kaydı
CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    actor_id    bigint REFERENCES users(id),
    actor_role  text,
    action      text  NOT NULL,
    entity      text  NOT NULL,
    entity_id   text,
    old_value   jsonb,
    new_value   jsonb,
    request_id  text
);
CREATE INDEX audit_log_entity_idx ON audit_log (entity, entity_id, id DESC);
CREATE INDEX audit_log_actor_idx  ON audit_log (actor_id, id DESC);

CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only (% not allowed)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();

-- Idempotency-Key: aynı kullanıcı + anahtar için ilk cevabı saklar.
CREATE TABLE idempotency_keys (
    user_id       bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key           text        NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
    method        text        NOT NULL,
    path          text        NOT NULL,
    request_hash  bytea       NOT NULL,
    status_code   int,
    response_body bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,
    PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);
