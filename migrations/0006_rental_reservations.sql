ALTER TABLE rentals
    ADD COLUMN reservation_expires_at timestamptz;

UPDATE rentals
   SET reservation_expires_at = reserved_at + interval '15 minutes'
 WHERE reservation_expires_at IS NULL;

ALTER TABLE rentals
    ALTER COLUMN reservation_expires_at SET NOT NULL,
    ADD CONSTRAINT rentals_reservation_expiry_check CHECK (reservation_expires_at > reserved_at);

ALTER TABLE orders
    ADD COLUMN idempotency_key text,
    ADD COLUMN order_status text NOT NULL DEFAULT 'PENDING';

UPDATE orders
   SET idempotency_key = gen_random_uuid()::text
 WHERE idempotency_key IS NULL;

ALTER TABLE orders
    ALTER COLUMN idempotency_key SET NOT NULL,
    ADD CONSTRAINT orders_idempotency_key_check CHECK (btrim(idempotency_key) <> '' AND length(idempotency_key) <= 200),
    ADD CONSTRAINT orders_status_check CHECK (order_status IN ('PENDING', 'EXPIRED'));

CREATE UNIQUE INDEX orders_user_idempotency_key
    ON orders (user_id, idempotency_key);

CREATE OR REPLACE FUNCTION prevent_order_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR
       NEW.user_id IS DISTINCT FROM OLD.user_id OR
       NEW.rental_plan_id IS DISTINCT FROM OLD.rental_plan_id OR
       NEW.plan_code_snapshot IS DISTINCT FROM OLD.plan_code_snapshot OR
       NEW.plan_name_snapshot IS DISTINCT FROM OLD.plan_name_snapshot OR
       NEW.duration_seconds_snapshot IS DISTINCT FROM OLD.duration_seconds_snapshot OR
       NEW.price_minor_units_snapshot IS DISTINCT FROM OLD.price_minor_units_snapshot OR
       NEW.currency_snapshot IS DISTINCT FROM OLD.currency_snapshot OR
       NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR
       NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'order identity and pricing snapshot are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'orders_immutable';
    END IF;

    IF NEW.order_status = OLD.order_status THEN
        RETURN NEW;
    END IF;

    IF OLD.order_status = 'PENDING' AND NEW.order_status = 'EXPIRED' THEN
        IF EXISTS (
            SELECT 1 FROM rentals
             WHERE order_id = NEW.id
               AND (ended_at IS NULL OR activated_at IS NOT NULL)
        ) OR NOT EXISTS (SELECT 1 FROM rentals WHERE order_id = NEW.id) THEN
            RAISE EXCEPTION 'only a closed, never-activated reservation can expire an order'
                USING ERRCODE = '23514', CONSTRAINT = 'orders_status_transition';
        END IF;
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'invalid order status transition: % -> %', OLD.order_status, NEW.order_status
        USING ERRCODE = '23514', CONSTRAINT = 'orders_status_transition';
END;
$$;

UPDATE orders o
    SET order_status = 'EXPIRED'
  FROM rentals r
 WHERE r.order_id = o.id
    AND r.ended_at IS NOT NULL
    AND r.activated_at IS NULL
    AND o.order_status = 'PENDING';

CREATE OR REPLACE FUNCTION assert_provider_number_rental_state(number_id uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    number_status text;
    open_rentals bigint;
    active_rentals bigint;
    invalid_order_state boolean;
BEGIN
    SELECT status INTO number_status FROM provider_numbers WHERE id = number_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;

        SELECT count(*) FILTER (WHERE ended_at IS NULL),
            count(*) FILTER (WHERE ended_at IS NULL AND activated_at IS NOT NULL),
           COALESCE(bool_or(
               (ended_at IS NULL AND activated_at IS NULL AND o.order_status <> 'PENDING') OR
               (ended_at IS NOT NULL AND activated_at IS NULL AND o.order_status <> 'EXPIRED')
           ), false)
      INTO open_rentals, active_rentals, invalid_order_state
      FROM rentals r
      JOIN orders o ON o.id = r.order_id
     WHERE r.provider_number_id = number_id;

    IF (number_status = 'RESERVED' AND (open_rentals <> 1 OR active_rentals <> 0)) OR
       (number_status = 'ACTIVE' AND (open_rentals <> 1 OR active_rentals <> 1)) OR
       (number_status NOT IN ('RESERVED', 'ACTIVE') AND open_rentals <> 0) OR
       invalid_order_state THEN
        RAISE EXCEPTION 'provider number % status conflicts with its rental or order state', number_id
            USING ERRCODE = '23514', CONSTRAINT = 'provider_number_rental_state';
    END IF;
END;
$$;

CREATE CONSTRAINT TRIGGER orders_rental_state_trigger
AFTER UPDATE OF order_status ON orders
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION check_provider_number_rental_state_trigger();

CREATE OR REPLACE FUNCTION check_provider_number_rental_state_trigger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    number_id uuid;
BEGIN
    IF TG_TABLE_NAME = 'provider_numbers' THEN
        number_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
    ELSIF TG_TABLE_NAME = 'rentals' THEN
        IF TG_OP = 'DELETE' THEN
            PERFORM assert_provider_number_rental_state(OLD.provider_number_id);
        ELSIF TG_OP = 'UPDATE' THEN
            PERFORM assert_provider_number_rental_state(OLD.provider_number_id);
            IF NEW.provider_number_id IS DISTINCT FROM OLD.provider_number_id THEN
                PERFORM assert_provider_number_rental_state(NEW.provider_number_id);
            END IF;
            RETURN NULL;
        ELSE
            PERFORM assert_provider_number_rental_state(NEW.provider_number_id);
        END IF;
        RETURN NULL;
    ELSE
        SELECT provider_number_id INTO number_id
          FROM rentals
         WHERE order_id = CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
        IF NOT FOUND THEN
            RETURN NULL;
        END IF;
    END IF;

    PERFORM assert_provider_number_rental_state(number_id);
    RETURN NULL;
END;
$$;