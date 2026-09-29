# Verifyflow Backend Milestones

## Milestone 1 - Financial core — done

Delivered: wallets, an append-only ledger, provider-cost/customer-price separation, verified Paystack deposits, controlled refunds and adjustments, transaction history, and wallet-backed number purchasing.

- [internal/wallet/service.go](../internal/wallet/service.go): `Purchase` (debit) and `RefundPurchase` (compensating credit), both atomic balance+ledger writes on the append-only `wallet_transactions` table, keyed by `(wallet_id, idempotency_key)`.
- `walletpurchase.Service.Purchase` ([internal/walletpurchase](../internal/walletpurchase/service.go), `POST /api/v1/numbers/purchase`) composes `rental.Reserve` → `wallet.Purchase` → `fulfillment.Fulfill`, sharing one idempotency key across the reservation and the debit. A hard fulfillment failure (anything but `fulfillment.ErrPendingFulfillment`) triggers `wallet.RefundPurchase` keyed off `"refund:"+originalIdempotencyKey`, so a retried compensation is a no-op instead of a double credit.
- Pricing is admin-entered, not auto-computed: [migrations/0013_rental_plan_provider_cost.sql](../migrations/0013_rental_plan_provider_cost.sql) adds `rental_plans.provider_cost_minor_units` (what the provider actually charges) alongside `price_minor_units` (what the customer is charged). The admin looks up the provider's cost and sets the customer price themselves — no markup formula. Admin-only endpoints in [internal/rental](../internal/rental/handler.go): `GET/POST /api/v1/admin/rental-plans`, `PATCH /api/v1/admin/rental-plans/{id}/pricing`. `provider_cost_minor_units` is never returned by the customer-facing `GET /api/v1/rental-plans`.
- [migrations/0014_order_provider_cost_snapshot.sql](../migrations/0014_order_provider_cost_snapshot.sql) snapshots each order's provider cost at reservation time (immutable, like its price snapshot), surfaced only via the new admin-only `GET /api/v1/admin/orders/{id}` (includes a computed `margin_minor_units`). Never returned by the customer-facing `GET /api/v1/orders/{id}`.
- Concurrency-safety verified against a real Postgres instance in [internal/walletpurchase/service_integration_test.go](../internal/walletpurchase/service_integration_test.go): 8 concurrent retries of the same purchase request under an identical Idempotency-Key produce exactly one `purchase` ledger row and, when fulfillment fails permanently, exactly one `refund` row — never more. This test caught and fixed a real race in `wallet.Purchase` (the balance pre-check ran before the idempotency-key insert, so a retry after a successful debit could misreport `insufficient funds` instead of the correct idempotency conflict).
- Full unit + integration suite green (`go test ./...` against Postgres, `-race`).

Exit evidence: concurrency-safe balance tests ✓; duplicate payment and adjustment tests ✓; insufficient-funds tests ✓; refund tests ✓; race-enabled suite ✓; migration checks ✓.

## Milestone 2 - Trust and administration

Deliver customer suspension, the support role, mandatory admin MFA enforcement, privileged audit events, user/order/number/transaction administration, webhook inspection and safe replay.

Exit evidence: authorization matrix tests, suspended-user tests, MFA enforcement tests, immutable audit tests, and IDOR tests.

## Milestone 3 - Lifecycle operations

Deliver inventory synchronization, reservation cleanup, rental expiry/renewal and release, payment reconciliation, webhook retries, provider health checks, low-balance alerts, and retention jobs.

Exit evidence: deterministic clock tests, retry/backoff tests, overlapping-worker safety tests, and lifecycle transition tests.

### Rental auto-renewal — done (pulled forward, since it's the natural follow-on to wallet-backed purchases)

Provider billing (Twilio/Vonage/Telnyx) is monthly-recurring per number, never one-time. Without this, Verifyflow would keep paying the provider for a number every month even after a customer stopped paying Verifyflow for it.

- [migrations/0015_rental_renewals.sql](../migrations/0015_rental_renewals.sql) adds `rentals.renewal_count` and allows `wallet_transactions.transaction_type = 'renewal'` (distinct from `'purchase'` so it's clear in a customer's history which charge is which).
- [internal/wallet/service.go](../internal/wallet/service.go): `Renew` — same debit as `Purchase`, posted under the `'renewal'` ledger type. Repository-level `Purchase` and `Renew` now share one `debit` helper to avoid duplicating the atomic balance+ledger logic.
- [internal/rental](../internal/rental/repository.go): `ListDueRenewals` (ACTIVE rentals past `expires_at`), `RenewRental` (extends `expires_at` by one more period), `ExpireActiveRental` (ends the rental and flips its number to `EXPIRED`) — both writes are optimistic-concurrency-guarded on `renewal_count` so two overlapping runs can't double-act on the same billing period.
- `renewal.Service.RunDue` ([internal/renewal](../internal/renewal/service.go)) ties it together: for every due rental, debit the customer's wallet for another period at the original price; on success extend the rental, on failure (insufficient funds, most commonly) release it. Each attempt's idempotency key is `rentalID + ":renewal:" + renewalCount`, so a retried run never double-charges or double-releases the same period.
- Runs automatically every `RENEWAL_INTERVAL` (default 1h, [cmd/api/main.go](../cmd/api/main.go)) via a background goroutine tied to the server's own shutdown context. Also reachable on demand via the admin-only `POST /api/v1/admin/renewals/run`, for ops/manual recovery.
- Integration-tested against real Postgres ([internal/renewal/service_integration_test.go](../internal/renewal/service_integration_test.go)): a funded wallet renews and extends the rental; an underfunded wallet is left untouched and the rental/number are released instead.

## Milestone 4 - Customer operations and production readiness

Deliver support tickets, notification preferences and delivery, administrative alerts, structured operational metrics, backup/restore procedures, webhook-burst and inbox load tests, and launch documentation.

Exit evidence: notification preference tests, support authorization tests, redaction checks, restore rehearsal, load-test results, and end-to-end acceptance evidence.

