CREATE TABLE wallet_funding_attempts (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id             uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    user_id               uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    provider_config_id    uuid        NOT NULL,
    provider_kind         text        NOT NULL DEFAULT 'payment',
    provider_reference    text,
    amount_minor_units    bigint      NOT NULL,
    currency              text        NOT NULL,
    status                text        NOT NULL DEFAULT 'initializing',
    idempotency_key       text        NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallet_funding_kind_check CHECK (provider_kind = 'payment'),
    CONSTRAINT wallet_funding_amount_check CHECK (amount_minor_units > 0),
    CONSTRAINT wallet_funding_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallet_funding_status_check CHECK (status IN ('initializing','pending','success','failed','abandoned','amount_mismatch')),
    CONSTRAINT wallet_funding_idempotency_check CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT wallet_funding_wallet_user_currency_fkey FOREIGN KEY (wallet_id,user_id,currency)
        REFERENCES wallets (id,user_id,currency) ON DELETE RESTRICT,
    CONSTRAINT wallet_funding_config_kind_fkey FOREIGN KEY (provider_config_id,provider_kind)
        REFERENCES provider_configs (id,provider_kind) ON DELETE RESTRICT,
    CONSTRAINT wallet_funding_user_idempotency_key UNIQUE (user_id,idempotency_key),
    CONSTRAINT wallet_funding_provider_reference_key UNIQUE (provider_config_id,provider_reference)
);

CREATE INDEX wallet_funding_user_created_idx ON wallet_funding_attempts (user_id,created_at DESC);

