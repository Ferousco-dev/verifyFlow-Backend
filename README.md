# Verifyflow Backend

Verifyflow Backend is a Go API backed by PostgreSQL. It provides account authentication and security features, and contains the internal service foundation for a phone-number rental product. The API can create and secure user accounts today; number rental and payment workflows are not yet exposed as public HTTP endpoints.

## What It Does

- Registers users and supports email/password sign-in.
- Issues short-lived JWT access tokens and rotating, single-use refresh tokens.
- Supports sign-out, password reset, email verification, and password changes.
- Sends transactional email through Resend when configured; without Resend configuration, development logs email content instead of delivering it.
- Includes internal telephony interfaces and a Twilio adapter for number search, purchase/release, webhook configuration, outbound SMS, and signed inbound SMS parsing.
- Includes PostgreSQL foundations for number inventory, rental plans, reservations, immutable priced orders, payment attempts, inbound messages, and provider webhook events.

The rental service creates a pending, server-priced order and reserves a number transactionally. It is not connected to public HTTP routes or a payment gateway yet. There is no wallet or stored user balance.

## Technology

- Go 1.22+
- PostgreSQL 13 or newer
- `pgx` for PostgreSQL access
- JWT bearer authentication, Argon2id password hashing, and Resend for transactional email
- Twilio Go request validation for signed inbound messaging webhooks

## Run Locally

Install Go and PostgreSQL, then create a development database:

```sh
createdb migo
cp .env.example .env
```

Set `DATABASE_URL` in `.env` to a connection string for that database and generate an access-token secret:

```sh
openssl rand -hex 32
```

Put the generated value in `ACCESS_TOKEN_SECRET`, then start the API:

```sh
go run ./cmd/api
```

In development, database migrations run automatically. To apply them explicitly:

```sh
go run ./cmd/api migrate
```

The local API listens on port 8080 by default. Check the database-backed health endpoint with `http://localhost:8080/health`.

### Docker Compose

Docker Compose starts PostgreSQL and the API:

```sh
export ACCESS_TOKEN_SECRET="$(openssl rand -hex 32)"
docker compose up --build
```

## API Documentation

- Interactive Swagger UI: `http://localhost:8080/docs/`
- OpenAPI definition: `http://localhost:8080/docs/openapi.yaml`
- Human-readable API reference: [docs/API.md](docs/API.md)

The public API currently includes health, authentication, and account endpoints. The OpenAPI specification lists only routes registered by the server.

## Configuration

Configuration is read from environment variables; a local `.env` file is loaded when present. See [.env.example](.env.example) for all supported settings.

Important settings:

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | PostgreSQL connection string |
| `ACCESS_TOKEN_SECRET` | Secret used to sign access tokens; use at least 32 characters |
| `PORT` | HTTP listen port; defaults to `8080` |
| `FRONTEND_URL` | Allowed browser origins and default frontend links |
| `RESEND_API_KEY`, `MAIL_FROM` | Transactional email delivery outside development |
| `TRUSTED_PROXIES` | Proxy IPs/CIDRs allowed to provide the client IP via forwarding headers |
| `RATE_LIMIT_ENABLED` | Enables in-memory authentication rate limits; defaults to `true` |
| `AUTO_MIGRATE` | Controls startup migrations; defaults on in development and off otherwise |

For production, use HTTPS, provide a strong secret, configure Resend and `MAIL_FROM`, and set `TRUSTED_PROXIES` to the actual proxy addresses when running behind a load balancer. The API requires Resend configuration outside development. Provider credential storage is designed for ciphertext; encryption/key management and provider configuration are not wired into runtime yet.

## Tests and Checks

Run unit and non-database tests with:

```sh
go test ./...
```

Database integration tests use a real PostgreSQL database. The test helper creates a separate temporary schema for each test and drops it afterward; the database role needs permission to create schemas.

```sh
createdb migo_test
export TEST_DATABASE_URL="postgresql://${USER}@localhost:5432/migo_test?sslmode=disable"
go test -race ./... -count=1
```

Run the project's full local checks with:

```sh
gofmt -l .
go vet ./...
go test -race ./... -count=1
go build ./...
```

Without `TEST_DATABASE_URL`, database integration tests are skipped locally. In CI, the test helper treats a missing database URL as an error.

## Project Structure

```text
cmd/api/             API entry point and process lifecycle
docs/                OpenAPI specification, Swagger UI, and API reference
internal/auth/       Authentication, tokens, sessions, and account security
internal/config/     Environment configuration and validation
internal/database/   PostgreSQL connection and migration runner
internal/httpx/      JSON, errors, middleware, CORS, and client IP handling
internal/mailer/     Email providers and asynchronous delivery queue
internal/rental/     Rental reservation service and PostgreSQL repository
internal/server/     HTTP routes and middleware wiring
internal/telephony/  Provider-neutral telephony contract and Twilio adapter
internal/user/       User model and repository
internal/testutil/   Isolated PostgreSQL integration-test setup
migrations/          Ordered SQL database migrations
```
# PRs are opened


