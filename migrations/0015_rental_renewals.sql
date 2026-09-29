ALTER TABLE rentals ADD COLUMN renewal_count integer NOT NULL DEFAULT 0;
ALTER TABLE rentals ADD CONSTRAINT rentals_renewal_count_check CHECK (renewal_count >= 0);

COMMENT ON COLUMN rentals.renewal_count IS
    'How many billing periods beyond the first this rental has been auto-renewed for. Part of the idempotency key for each renewal debit, so a retried renewal run never double-charges a period.';

ALTER TABLE wallet_transactions DROP CONSTRAINT wallet_transactions_type_check;
ALTER TABLE wallet_transactions ADD CONSTRAINT wallet_transactions_type_check
    CHECK (transaction_type IN ('deposit','purchase','refund','adjustment_credit','adjustment_debit','renewal'));
