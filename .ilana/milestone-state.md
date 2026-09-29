# Milestone state: milestone-1-financial-core

Ceremony standard. Step: construction. Base `0bc294f`, head `b4ef47d` on `main`; 23 dirty path(s).

## Completed
- REQ-001 through REQ-006: provider routing adapters signed inbound processing normalized messages inbox number listing and outbound SMS.
- Defined four remaining backend milestones in docs/BACKEND_MILESTONES.md.
- Created currency-scoped wallets and an append-only transaction ledger.
- Added authenticated wallet balance and transaction-history endpoints.
- Added admin-only idempotent credit/debit adjustments with mandatory reasons and overdraft prevention.
- Implemented credential-optional Paystack wallet funding initialization and authenticated verification.
- Integrated wallet funding references into the existing signed Paystack webhook path.
- Added atomic exactly-once deposit settlement and amount/currency mismatch rejection.
- Expanded .env and .env.example with all runtime settings and blank Paystack Twilio Vonage Telnyx credential placeholders.

## Verified behavior
- EV-002 go vet passed.
- EV-003 race-enabled full test suite passed.
- EV-004 full build passed.
- EV-005 milestone vet passed.
- EV-006 race-enabled full suite passed with wallet integration tests.
- EV-007 milestone build passed.
- EV-008 Paystack funding vet passed.
- EV-009 race-enabled full suite passed including exact-once funding integration tests.
- EV-010 Paystack funding build passed.
- Environment templates contain no duplicate active variable names; EV-011 full tests passed.

## Remaining
- REQ-007 wallet and immutable financial ledger APIs.
- REQ-008 background lifecycle reconciliation retry and provider-health workers.
- REQ-009 complete administrative reporting and webhook replay APIs.
- REQ-010 support role suspension admin MFA and privileged audit events.
- REQ-011 configurable pricing engine.
- REQ-012 support tickets notification preferences and notification delivery.
- NFR-003 production observability backup restore and load-test evidence.
- Milestone 1: verified Paystack wallet deposits and duplicate webhook credit protection.
- Milestone 1: wallet-backed number purchasing refunds and compensating reversals.
- Milestone 1: provider-cost and customer-price rules.

## Known limitations
- Provider live-account credentials and production certification are unavailable; adapters can be verified with deterministic HTTP fixtures only.
- Live provider certification requires verified funded Twilio Vonage and Telnyx accounts.
- Paystack checkout cannot run against the live service until an enabled encrypted provider configuration is supplied.
- Provider credential environment variables are staging placeholders; production credentials are stored through encrypted provider_configs and are not read directly by provider adapters.

## Repository contradicted an earlier claim
- Previous handoff recorded head 0bc294f with provider work uncommitted; repository now contains commit b4ef47d adding that provider work. Continued from repository head b4ef47d.

## Stop boundary (do not start)
- frontend implementation
- production deployment

## Next action
Implement wallet-backed number purchasing and compensating refunds then add pricing rules.

## Latest evidence
| id | label | result | command |
| --- | --- | --- | --- |
| EV-004 | build | PASS | go build ./... |
| EV-011 | env-template-tests | PASS | go test ./... |
| EV-007 | milestone-1-build | PASS | go build ./... |
| EV-010 | milestone-1-funding-build | PASS | go build ./... |
| EV-009 | milestone-1-funding-tests | PASS | go test -race ./... -count=1 |
| EV-008 | milestone-1-funding-vet | PASS | go vet ./... |
| EV-006 | milestone-1-tests | PASS | go test -race ./... -count=1 |
| EV-005 | milestone-1-vet | PASS | go vet ./... |
| EV-003 | tests | PASS | go test -race ./... -count=1 |
| EV-002 | vet | PASS | go vet ./... |

Rendered from `.ilana/handoff.json`. Edit with `evidence.py handoff`, not by hand.
