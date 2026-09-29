ALTER TABLE rental_plans ADD COLUMN provider_cost_minor_units bigint NOT NULL DEFAULT 0;

ALTER TABLE rental_plans
    ADD CONSTRAINT rental_plans_provider_cost_check CHECK (provider_cost_minor_units >= 0);

COMMENT ON COLUMN rental_plans.provider_cost_minor_units IS
    'What the telephony provider charges Migo for this plan; admin-entered, never shown to customers. price_minor_units is what Migo charges the customer.';
