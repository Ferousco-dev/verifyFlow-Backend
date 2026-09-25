CREATE TABLE refresh_sessions (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- All tokens descended from one login share a family (one device/login).
    family_id      uuid        NOT NULL,
    -- SHA-256 of the raw refresh token. The raw token is never stored.
    token_hash     bytea       NOT NULL,
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz,
    revoked_at     timestamptz,
    revoked_reason text,
    replaced_by    uuid        REFERENCES refresh_sessions(id),
    user_agent     text        NOT NULL DEFAULT '',
    ip             text        NOT NULL DEFAULT '',
    CONSTRAINT refresh_sessions_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX refresh_sessions_user_id_idx   ON refresh_sessions (user_id);
CREATE INDEX refresh_sessions_family_id_idx ON refresh_sessions (family_id);
CREATE INDEX refresh_sessions_expires_at_idx ON refresh_sessions (expires_at);
