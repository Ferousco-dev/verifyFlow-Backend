# Migo backend 


Go + PostgreSQL. Email/password auth, Argon2id, short-lived JWT access tokens,
rotating refresh sessions with reuse detection, email verification, and account password changes.
**No wallet. No Twilio/Paystack/Redis yet.**

## Run it

```bash
cd backend
go mod tidy                      # first time: resolves deps, writes go.sum
createdb migo                    # local Postgres (Postgres 13+)
cp .env.example .env
# edit .env: set DATABASE_URL username and:
openssl rand -hex 32             # paste into ACCESS_TOKEN_SECRET
go run ./cmd/api                 # migrations run automatically in development
```

Or with Docker (Postgres + API):

```bash
export ACCESS_TOKEN_SECRET=$(openssl rand -hex 32)
docker compose up --build
```

Manual migrations: `go run ./cmd/api migrate`.

Checks: `gofmt -l . && go vet ./... && go test -race ./... && go build ./...`

### Integration tests (real PostgreSQL)

Repository, migration and service tests run against a real database. Without
`TEST_DATABASE_URL` they are **skipped** locally (unit tests still run); in CI a missing
variable is a hard failure so the suite can't silently stop covering the database.

```bash
createdb migo_test
export TEST_DATABASE_URL='postgresql://YOUR_LOCAL_USERNAME@localhost:5432/migo_test?sslmode=disable'
go test -race -count=1 ./...
```

Or with the compose database: `docker compose up -d db` and use
`postgresql://migo:migo@localhost:5432/migo?sslmode=disable`.

Each test creates its own uniquely named schema (`t_<random>`), runs the migrations
inside it, and drops it afterwards. Tests never touch the `public` schema or your dev data,
so they are safe to run against your normal local database, and they run in parallel.
The role needs `CREATE` on the database.

What they cover:

- **Migrations:** idempotent re-run, 6 concurrent runners (advisory lock), failed migration rolls back atomically.
- **Server (full stack):** register/login rate limits, spoofed `X-Forwarded-For` ignored, trusted-proxy handling, per-account login lockout can't be triggered from another IP, CORS preflight and 429 headers, session records the resolved client IP.
- **Password reset:** token stored as digest, one live token per user, full-effect atomic consume, failed attempts have no side effects, 10-way concurrent race → one winner, deterministic row-lock proof, end-to-end flow (register → forgot → email → reset → old password/sessions dead), no account enumeration, per-address and per-IP limits.
- **Email verification and change password:** full-stack router tests for registration/resend/verification and password change, token replacement/single-use, verified state, session revocation, fresh tokens, and old/new password behavior.
- **Mailer:** SMTP adapter wire format against a fake server, header-injection rejection, credentials refused without TLS, async queue never blocks, drains on shutdown, survives panics.
- **Resend:** HTTP API payload/auth, plain-text delivery, safe status-only API errors, header-injection rejection.
- **Users:** round-trip, NULL password hash, uniqueness → `ErrEmailTaken`/`ErrUsernameTaken`, CHECK constraints (lowercase email, username format), 20-way concurrent insert races (exactly one winner).
- **Refresh sessions:** only a 32-byte digest is stored, rotation chain and family, not-found/expired/revoked outcomes, reuse revokes the whole family *and the revocation is committed*, deactivated users, logout scoped to one device, FK cascade, 20-way concurrent rotation with a single winner, and a deterministic test that `Rotate` serializes on the row lock (`SELECT … FOR UPDATE`).
- **Service end-to-end:** full register → login → refresh → reuse → logout flow on real repositories, concurrent same-name registrations get unique usernames, concurrent same-email registration has one winner, password-hash upgrade persists.

Smoke test:

```bash
curl -s localhost:8080/health
curl -s -X POST localhost:8080/api/v1/auth/register -H 'Content-Type: application/json' \
  -d '{"full_name":"Feranmi Oresajo","email":"fer@example.com","password":"a-long-password"}'
```

API reference: [docs/API.md](docs/API.md)
Interactive Swagger UI: `http://localhost:8080/docs/` (or `/docs/` on the deployed API).

## Layout

```
cmd/api            entrypoint (config, DB, migrations, graceful shutdown)
internal/config    env parsing + validation
internal/database  pgx pool + embedded-SQL migration runner
internal/httpx     JSON helpers, error envelope, logging/recover, CORS, trusted-proxy client IP
internal/mailer    Sender interface: Resend API, SMTP adapter, dev log sender, async queue
internal/ratelimit token-bucket limiter (in-memory, Redis-swappable interface) + middleware
internal/user      user model, username slugging, repository
internal/testutil  dbtest: per-test isolated schema for integration tests
internal/auth      Argon2id, JWT, refresh sessions, service, handlers, middleware
internal/server    routes
migrations         0001_users, 0002_refresh_sessions, 0003_password_reset_tokens, 0004_email_verification_tokens
```

## Hardening (v0.2)

- **Rate limiting** on register, login, refresh and logout (see `docs/API.md`). In-memory token
  buckets; bounded memory; IPv6 clients are bucketed by /64. Login also has a per-(IP, email)
  budget that a successful login clears, so attackers can't lock real users out. The `Limiter`
  interface is what a Redis implementation will satisfy when you run more than one instance.
  Limits apply per process until then.
- **CORS** allow-list from `FRONTEND_URL` (validated: exact origins only, no `*`, https required
  outside development except loopback). No credentialed CORS; Bearer tokens only.
- **Client IP** comes from `RemoteAddr`. `X-Forwarded-For` is only trusted from proxies listed in
  `TRUSTED_PROXIES`, and only the right-most untrusted hop is used, so clients can't spoof it to evade limits.
  **Behind a load balancer you must set `TRUSTED_PROXIES`**, otherwise all users share the proxy's IP
  and will rate-limit each other.
- Set `RATE_LIMIT_ENABLED=false` only for load tests.

## Password reset (v0.3)

`POST /auth/forgot-password` then `POST /auth/reset-password` (see `docs/API.md`).

- **No account enumeration:** forgot-password answers `202` with the same body for real, unknown and
  deactivated addresses. Email is sent on a background queue, so response time doesn't leak
  either. (A real account costs one extra DB insert, a few milliseconds; negligible next to network jitter.)
- **Tokens:** 256-bit random, only the SHA-256 is stored, single-use, 30 min TTL, one live token per
  user (a new request cancels the old link). Delivered in the URL **fragment** (`#token=`) so it never
  hits server or proxy logs; the frontend must strip it from the address bar.
- **Consuming a token is one transaction:** set password, mark email verified, mark token used, delete
  sibling tokens, **revoke every refresh session** (so anyone holding a stolen session, or who
  pre-registered your email, is evicted). `SELECT ... FOR UPDATE` makes concurrent use of one link
  produce exactly one winner (tested with a deterministic row-lock test).
- A too-weak new password is rejected **before** the token is spent, so the link survives a typo.
- Abuse limits: per-IP, plus a per-address cap so nobody can mail-bomb a victim by rotating IPs. When the
  per-address cap is hit the API still answers `202` (silently no email), so the limit itself leaks nothing.
  Trade-off: an attacker can burn a victim's hourly reset budget; they can still reset later or use a fresh link.
- A "password changed" notice goes to the account's email after every successful reset.
- **Email delivery:** Resend is the production provider. Set `RESEND_API_KEY` and `MAIL_FROM` to a
  sender address on a domain verified in Resend; the server refuses to start outside development
  without the API key. Delivery runs on the bounded background queue. In development, omit the key
  to log emails to the console (including live links, so keep those logs private). The `mailer.Sender`
  interface keeps providers replaceable.
- Reset is also how an OAuth-only account (future Google sign-in) can set a first password.

## Account security (v0.4)

- **Email verification:** registration sends a 24-hour single-use link. Only a SHA-256 digest is
  stored; links are bound to the issued email address, and resending replaces the previous link.
  `POST /auth/verify-email` consumes the token and marks the address verified atomically.
  `POST /auth/resend-verification` requires sign-in and is limited per IP and user.
- **Change password:** `POST /auth/change-password` verifies the current password and uses a
  database compare-and-swap so an in-flight stale request cannot overwrite a password changed by
  reset or another request. Success revokes every refresh session, deletes pending reset links,
  creates a replacement session for the caller, and emails a change notice. The caller receives
  fresh access and refresh tokens and stays signed in.
- Both account-security flows have full-stack HTTP integration tests backed by the same isolated
  real-PostgreSQL schema harness as the repository tests.

## Design decisions

- **Users vs. identities:** `users.password_hash` is nullable and lives on `users` for now.
  A separate `auth_identities` table (provider, subject) is the right shape once Google OAuth
  lands; adding it later is an additive migration, so it isn't built yet.
- **Refresh tokens:** 256-bit random, only the SHA-256 digest is stored. Each login starts a
  *family*. Refresh rotates within the family; replaying an already-rotated token revokes the
  whole family. A client that fires two simultaneous refreshes with the same token will be
  signed out — clients must serialize refresh calls.
- **JWT:** HS256, stdlib only, strict header check (rejects `alg:none`), claims are
  `iss/sub/iat/exp` only. Access tokens are stateless: a logout takes effect at the next
  refresh, up to `ACCESS_TOKEN_TTL` (15 min) later.
- **Username uniqueness:** DB `UNIQUE` is authoritative; on conflict the service retries with a
  random numeric suffix.
- **Login:** identical error for unknown email / wrong password / inactive account, and an Argon2
  verification always runs so timing doesn't reveal account existence.
- **Registration** does reveal that an email is taken (409). Email verification protects account
  ownership, but registration still returns this conflict to let the user sign in or recover access.
- **Client IP** stored on sessions is the same resolved IP used for rate limiting (see Hardening).
# verifyFlow-Backend
