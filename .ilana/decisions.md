# Decisions

## DEC-001 - Multi-provider routing

Inventory retains its provider configuration identity. Provisioning and messaging resolve that exact configuration instead of selecting the first enabled telecom vendor. Supported keys are `twilio`, `vonage`, and `telnyx`.

## DEC-002 - Risk and ceremony

Rigour is 4 because the service handles money, phone identifiers, credentials, and private message content. Standard ceremony was selected by the accepted defaults; security-sensitive behavior still requires direct tests and evidence.

## DEC-003 - Delivery boundary

This change is backend-only. Frontend implementation and production deployment remain outside the milestone. Live provider certification requires separately supplied verified accounts and secrets.

## DEC-004 - Message normalization

Provider webhooks are verified by their adapters and written into a common `messages` model. Provider payload bodies are not persisted by the messaging service; only correlation metadata is retained to reduce sensitive-data exposure.

## DEC-005 - Optional Paystack runtime configuration

Wallet funding is implemented without embedding credentials. The existing encrypted provider configuration resolves Paystack only at request time. Until an enabled configuration is supplied, funding endpoints fail closed with a service-unavailable response; tests use a local deterministic HTTP provider double.
