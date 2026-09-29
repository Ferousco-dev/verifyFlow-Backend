ALTER TABLE orders ADD COLUMN provider_cost_minor_units_snapshot bigint NOT NULL DEFAULT 0;

ALTER TABLE orders
    ADD CONSTRAINT orders_provider_cost_snapshot_check CHECK (provider_cost_minor_units_snapshot >= 0);

COMMENT ON COLUMN orders.provider_cost_minor_units_snapshot IS
    'Snapshot of rental_plans.provider_cost_minor_units at reservation time, admin-only; price_minor_units_snapshot is what the customer was charged.';
