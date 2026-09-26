CREATE TABLE provider_configs (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_kind          text        NOT NULL,
    provider_key           text        NOT NULL,
    config_name            text        NOT NULL,
    credentials_ciphertext bytea       NOT NULL,
    credential_key_version text        NOT NULL,
    is_enabled             boolean     NOT NULL DEFAULT true,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT provider_configs_kind_check CHECK (provider_kind IN ('telephony', 'payment')),
    CONSTRAINT provider_configs_key_check CHECK (provider_key ~ '^[a-z][a-z0-9_-]{0,63}$'),
    CONSTRAINT provider_configs_name_check CHECK (btrim(config_name) <> ''),
    CONSTRAINT provider_configs_ciphertext_check CHECK (octet_length(credentials_ciphertext) > 0),
    CONSTRAINT provider_configs_key_version_check CHECK (btrim(credential_key_version) <> ''),
    CONSTRAINT provider_configs_name_key UNIQUE (provider_kind, provider_key, config_name),
    CONSTRAINT provider_configs_id_kind_key UNIQUE (id, provider_kind)
);

COMMENT ON COLUMN provider_configs.credentials_ciphertext IS
    'Authenticated-encryption envelope only; never store plaintext provider credentials here.';

CREATE TABLE provider_numbers (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_config_id uuid        NOT NULL,
    provider_kind      text        NOT NULL DEFAULT 'telephony',
    provider_reference text        NOT NULL,
    phone_number       text        NOT NULL,
    number_type        text        NOT NULL,
    sms_enabled        boolean     NOT NULL DEFAULT false,
    mms_enabled        boolean     NOT NULL DEFAULT false,
    voice_enabled      boolean     NOT NULL DEFAULT false,
    status             text        NOT NULL DEFAULT 'AVAILABLE',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT provider_numbers_kind_check CHECK (provider_kind = 'telephony'),
    CONSTRAINT provider_numbers_provider_ref_check CHECK (btrim(provider_reference) <> ''),
    CONSTRAINT provider_numbers_phone_format_check CHECK (phone_number ~ '^\+[1-9][0-9]{1,14}$'),
    CONSTRAINT provider_numbers_type_check CHECK (btrim(number_type) <> ''),
    CONSTRAINT provider_numbers_status_check CHECK (status IN (
        'AVAILABLE', 'RESERVED', 'ACTIVE', 'EXPIRED', 'QUARANTINED', 'REVIEW', 'REUSABLE', 'RETIRED'
    )),
    CONSTRAINT provider_numbers_provider_ref_key UNIQUE (provider_config_id, provider_reference),
    CONSTRAINT provider_numbers_phone_key UNIQUE (provider_config_id, phone_number),
    CONSTRAINT provider_numbers_id_phone_key UNIQUE (id, phone_number),
    CONSTRAINT provider_numbers_config_kind_fkey FOREIGN KEY (provider_config_id, provider_kind)
        REFERENCES provider_configs (id, provider_kind) ON DELETE RESTRICT
);

COMMENT ON COLUMN provider_numbers.provider_reference IS
    'Opaque provider reference; provider_numbers.id remains the Migo identity.';

CREATE INDEX provider_numbers_status_idx ON provider_numbers (status);

CREATE FUNCTION enforce_provider_number_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'AVAILABLE' THEN
            RAISE EXCEPTION 'new provider numbers must start AVAILABLE'
                USING ERRCODE = '23514', CONSTRAINT = 'provider_numbers_status_transition';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF (OLD.status = 'AVAILABLE' AND NEW.status = 'RESERVED') OR
       (OLD.status = 'RESERVED' AND NEW.status IN ('ACTIVE', 'AVAILABLE')) OR
       (OLD.status = 'ACTIVE' AND NEW.status = 'EXPIRED') OR
       (OLD.status = 'EXPIRED' AND NEW.status = 'QUARANTINED') OR
       (OLD.status = 'QUARANTINED' AND NEW.status = 'REVIEW') OR
       (OLD.status = 'REVIEW' AND NEW.status IN ('REUSABLE', 'RETIRED')) OR
       (OLD.status = 'REUSABLE' AND NEW.status = 'AVAILABLE') THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'invalid provider number lifecycle transition: % -> %', OLD.status, NEW.status
        USING ERRCODE = '23514', CONSTRAINT = 'provider_numbers_status_transition';
END;
$$;

CREATE TRIGGER provider_numbers_status_transition_trigger
BEFORE INSERT OR UPDATE OF status ON provider_numbers
FOR EACH ROW EXECUTE FUNCTION enforce_provider_number_status_transition();

CREATE TABLE rental_plans (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_code         text        NOT NULL,
    name              text        NOT NULL,
    duration_seconds  integer     NOT NULL,
    price_minor_units bigint      NOT NULL,
    currency          text        NOT NULL,
    is_active         boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT rental_plans_code_key UNIQUE (plan_code),
    CONSTRAINT rental_plans_code_check CHECK (btrim(plan_code) <> ''),
    CONSTRAINT rental_plans_name_check CHECK (btrim(name) <> ''),
    CONSTRAINT rental_plans_duration_check CHECK (duration_seconds > 0),
    CONSTRAINT rental_plans_price_check CHECK (price_minor_units >= 0),
    CONSTRAINT rental_plans_currency_check CHECK (currency ~ '^[A-Z]{3}$')
);

CREATE INDEX rental_plans_active_idx ON rental_plans (plan_code) WHERE is_active;

CREATE TABLE orders (
    id                        uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                   uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    rental_plan_id            uuid        NOT NULL REFERENCES rental_plans (id) ON DELETE RESTRICT,
    plan_code_snapshot        text        NOT NULL,
    plan_name_snapshot        text        NOT NULL,
    duration_seconds_snapshot integer     NOT NULL,
    price_minor_units_snapshot bigint     NOT NULL,
    currency_snapshot         text        NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_id_user_key UNIQUE (id, user_id),
    CONSTRAINT orders_plan_code_snapshot_check CHECK (btrim(plan_code_snapshot) <> ''),
    CONSTRAINT orders_plan_name_snapshot_check CHECK (btrim(plan_name_snapshot) <> ''),
    CONSTRAINT orders_duration_snapshot_check CHECK (duration_seconds_snapshot > 0),
    CONSTRAINT orders_price_snapshot_check CHECK (price_minor_units_snapshot >= 0),
    CONSTRAINT orders_currency_snapshot_check CHECK (currency_snapshot ~ '^[A-Z]{3}$')
);

CREATE INDEX orders_user_created_at_idx ON orders (user_id, created_at DESC);

CREATE FUNCTION prevent_order_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'orders are immutable after creation'
        USING ERRCODE = '23514', CONSTRAINT = 'orders_immutable';
END;
$$;

CREATE TRIGGER orders_immutable_trigger
BEFORE UPDATE OR DELETE ON orders
FOR EACH ROW EXECUTE FUNCTION prevent_order_mutation();

CREATE TABLE rentals (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id           uuid        NOT NULL UNIQUE,
    user_id            uuid        NOT NULL,
    provider_number_id uuid        NOT NULL REFERENCES provider_numbers (id) ON DELETE RESTRICT,
    reserved_at        timestamptz NOT NULL DEFAULT now(),
    activated_at       timestamptz,
    expires_at         timestamptz,
    ended_at           timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT rentals_order_user_fkey FOREIGN KEY (order_id, user_id)
        REFERENCES orders (id, user_id) ON DELETE RESTRICT,
    CONSTRAINT rentals_id_provider_number_key UNIQUE (id, provider_number_id),
    CONSTRAINT rentals_activation_time_check CHECK (activated_at IS NULL OR activated_at >= reserved_at),
    CONSTRAINT rentals_expiry_time_check CHECK (expires_at IS NULL OR expires_at > COALESCE(activated_at, reserved_at)),
    CONSTRAINT rentals_end_time_check CHECK (ended_at IS NULL OR ended_at >= COALESCE(activated_at, reserved_at))
);

CREATE UNIQUE INDEX rentals_one_open_per_provider_number_key
    ON rentals (provider_number_id) WHERE ended_at IS NULL;
CREATE INDEX rentals_user_created_at_idx ON rentals (user_id, created_at DESC);

CREATE FUNCTION assert_provider_number_rental_state(number_id uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    number_status text;
    open_rentals bigint;
    active_rentals bigint;
BEGIN
    SELECT status INTO number_status FROM provider_numbers WHERE id = number_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT count(*), count(*) FILTER (WHERE activated_at IS NOT NULL)
    INTO open_rentals, active_rentals
    FROM rentals
    WHERE provider_number_id = number_id AND ended_at IS NULL;

    IF (number_status = 'RESERVED' AND (open_rentals <> 1 OR active_rentals <> 0)) OR
       (number_status = 'ACTIVE' AND (open_rentals <> 1 OR active_rentals <> 1)) OR
       (number_status NOT IN ('RESERVED', 'ACTIVE') AND open_rentals <> 0) THEN
        RAISE EXCEPTION 'provider number % status % conflicts with its open rentals', number_id, number_status
            USING ERRCODE = '23514', CONSTRAINT = 'provider_number_rental_state';
    END IF;
END;
$$;

CREATE FUNCTION check_provider_number_rental_state_trigger() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'provider_numbers' THEN
        IF TG_OP = 'DELETE' THEN
            PERFORM assert_provider_number_rental_state(OLD.id);
        ELSE
            PERFORM assert_provider_number_rental_state(NEW.id);
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM assert_provider_number_rental_state(OLD.provider_number_id);
    ELSIF TG_OP = 'UPDATE' THEN
        PERFORM assert_provider_number_rental_state(OLD.provider_number_id);
        IF NEW.provider_number_id IS DISTINCT FROM OLD.provider_number_id THEN
            PERFORM assert_provider_number_rental_state(NEW.provider_number_id);
        END IF;
    ELSE
        PERFORM assert_provider_number_rental_state(NEW.provider_number_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER provider_numbers_rental_state_trigger
AFTER INSERT OR UPDATE OR DELETE ON provider_numbers
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION check_provider_number_rental_state_trigger();

CREATE CONSTRAINT TRIGGER rentals_provider_number_state_trigger
AFTER INSERT OR UPDATE OR DELETE ON rentals
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION check_provider_number_rental_state_trigger();

CREATE TABLE payments (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id           uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    provider_config_id uuid        NOT NULL,
    provider_kind      text        NOT NULL DEFAULT 'payment',
    provider_reference text,
    provider_status    text        NOT NULL DEFAULT '',
    amount_minor_units bigint      NOT NULL,
    currency           text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payments_kind_check CHECK (provider_kind = 'payment'),
    CONSTRAINT payments_provider_ref_check CHECK (provider_reference IS NULL OR btrim(provider_reference) <> ''),
    CONSTRAINT payments_amount_check CHECK (amount_minor_units >= 0),
    CONSTRAINT payments_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_config_kind_fkey FOREIGN KEY (provider_config_id, provider_kind)
        REFERENCES provider_configs (id, provider_kind) ON DELETE RESTRICT,
    CONSTRAINT payments_provider_reference_key UNIQUE (provider_config_id, provider_reference)
);

CREATE INDEX payments_order_created_at_idx ON payments (order_id, created_at DESC);

CREATE TABLE inbound_messages (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    rental_id          uuid        NOT NULL,
    provider_number_id uuid        NOT NULL,
    provider_reference text        NOT NULL,
    from_number        text        NOT NULL,
    to_number          text        NOT NULL,
    body               text        NOT NULL DEFAULT '',
    received_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT inbound_messages_provider_ref_check CHECK (btrim(provider_reference) <> ''),
    CONSTRAINT inbound_messages_from_check CHECK (btrim(from_number) <> ''),
    CONSTRAINT inbound_messages_to_check CHECK (btrim(to_number) <> ''),
    CONSTRAINT inbound_messages_rental_number_fkey FOREIGN KEY (rental_id, provider_number_id)
        REFERENCES rentals (id, provider_number_id) ON DELETE RESTRICT,
    CONSTRAINT inbound_messages_to_number_fkey FOREIGN KEY (provider_number_id, to_number)
        REFERENCES provider_numbers (id, phone_number) ON DELETE RESTRICT,
    CONSTRAINT inbound_messages_rental_provider_ref_key UNIQUE (rental_id, provider_reference)
);

CREATE INDEX inbound_messages_rental_received_at_idx ON inbound_messages (rental_id, received_at DESC);

CREATE TABLE provider_webhook_events (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_config_id uuid        NOT NULL,
    provider_kind      text        NOT NULL,
    idempotency_key    text        NOT NULL,
    provider_reference text,
    event_type         text        NOT NULL,
    payload            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    received_at        timestamptz NOT NULL DEFAULT now(),
    processed_at       timestamptz,
    CONSTRAINT provider_webhook_events_kind_check CHECK (provider_kind IN ('telephony', 'payment')),
    CONSTRAINT provider_webhook_events_key_check CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT provider_webhook_events_type_check CHECK (btrim(event_type) <> ''),
    CONSTRAINT provider_webhook_events_payload_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT provider_webhook_events_processed_at_check CHECK (processed_at IS NULL OR processed_at >= received_at),
    CONSTRAINT provider_webhook_events_config_kind_fkey FOREIGN KEY (provider_config_id, provider_kind)
        REFERENCES provider_configs (id, provider_kind) ON DELETE RESTRICT,
    CONSTRAINT provider_webhook_events_idempotency_key UNIQUE (provider_config_id, idempotency_key)
);

CREATE INDEX provider_webhook_events_unprocessed_idx
    ON provider_webhook_events (received_at) WHERE processed_at IS NULL;