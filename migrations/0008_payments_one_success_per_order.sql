-- Multiple payment attempts per order are expected (retries, abandoned
-- checkouts), but only one may ever be recorded as successful, so a
-- provisioning flow reading "the" successful payment is never ambiguous.
CREATE UNIQUE INDEX payments_one_success_per_order_key
    ON payments (order_id) WHERE provider_status = 'success';
