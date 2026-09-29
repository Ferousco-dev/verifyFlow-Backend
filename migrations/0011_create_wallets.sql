CREATE TABLE wallets (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    currency              text        NOT NULL,
    available_minor_units bigint      NOT NULL DEFAULT 0,
    pending_minor_units   bigint      NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallets_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_available_nonnegative CHECK (available_minor_units >= 0),
    CONSTRAINT wallets_pending_nonnegative CHECK (pending_minor_units >= 0),
    CONSTRAINT wallets_user_currency_key UNIQUE (user_id, currency),
    CONSTRAINT wallets_id_user_currency_key UNIQUE (id, user_id, currency)
);

CREATE TABLE wallet_transactions (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id             uuid        NOT NULL REFERENCES wallets (id) ON DELETE RESTRICT,
    user_id               uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    transaction_type      text        NOT NULL,
    amount_minor_units    bigint      NOT NULL,
    currency              text        NOT NULL,
    reference             text        NOT NULL,
    idempotency_key       text        NOT NULL,
    status                text        NOT NULL DEFAULT 'posted',
    actor_user_id         uuid        REFERENCES users (id) ON DELETE RESTRICT,
    reason                text        NOT NULL DEFAULT '',
    metadata              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallet_transactions_type_check CHECK (transaction_type IN ('deposit','purchase','refund','adjustment_credit','adjustment_debit')),
    CONSTRAINT wallet_transactions_amount_positive CHECK (amount_minor_units > 0),
    CONSTRAINT wallet_transactions_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallet_transactions_reference_check CHECK (btrim(reference) <> ''),
    CONSTRAINT wallet_transactions_idempotency_check CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT wallet_transactions_status_check CHECK (status IN ('posted','reversed')),
    CONSTRAINT wallet_transactions_metadata_check CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT wallet_transactions_wallet_user_currency_fkey FOREIGN KEY (wallet_id, user_id, currency)
        REFERENCES wallets (id, user_id, currency) ON DELETE RESTRICT,
    CONSTRAINT wallet_transactions_idempotency_key UNIQUE (wallet_id, idempotency_key)
);

CREATE INDEX wallet_transactions_user_created_idx ON wallet_transactions (user_id, created_at DESC, id DESC);

CREATE FUNCTION prevent_wallet_transaction_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet transactions are append-only'
        USING ERRCODE = '23514', CONSTRAINT = 'wallet_transactions_immutable';
END;
$$;

CREATE TRIGGER wallet_transactions_immutable_trigger
BEFORE UPDATE OR DELETE ON wallet_transactions
FOR EACH ROW EXECUTE FUNCTION prevent_wallet_transaction_mutation();
