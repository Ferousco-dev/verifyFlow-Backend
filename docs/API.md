# Migo API — v0.4

Interactive Swagger UI: `/docs/` on the running API (for example, `http://localhost:8080/docs/`). The same path works after deployment. The machine-readable OpenAPI 3.0 specification is available at `/docs/openapi.yaml` and in [openapi.yaml](openapi.yaml).

This reference documents only routes currently registered by the server. Internal telephony/provider support and rental/order/payment persistence exist, but are not yet exposed as HTTP endpoints.

Base URL (local): `http://localhost:8080`
All bodies are JSON (`Content-Type: application/json`). Max request body: 16 KiB.
Unknown JSON fields are rejected. Every response includes an `X-Request-ID` header.

## Error format

Every error uses the same envelope:

```json
{ "error": { "code": "validation_failed", "message": "One or more fields are invalid.", "fields": { "email": "must be a valid email address" } } }
```

`fields` appears only for validation errors.

| HTTP | code | When |
|---|---|---|
| 400 | `invalid_json` | Malformed JSON, unknown fields, or trailing data |
| 401 | `unauthorized` | Missing/invalid/expired access token (also deactivated user) |
| 401 | `invalid_credentials` | Wrong email or password (identical for unknown email) |
| 401 | `invalid_refresh_token` | Unknown, expired, revoked, or reused refresh token |
| 409 | `email_taken` | Registration with an existing email |
| 403 | `cors_origin_not_allowed` | CORS preflight from an origin that isn't allow-listed |
| 400 | `invalid_reset_token` | Reset token unknown, expired, already used, or replaced by a newer one |
| 413 | `request_too_large` | Body over 16 KiB |
| 422 | `validation_failed` | Field validation failed |
| 429 | `rate_limited` | Too many requests; see `Retry-After` |
| 500 | `internal_error` | Unexpected server error (details are never exposed) |
| 504 | `timeout` | Request exceeded its 10s deadline |

## Rate limiting

Auth endpoints are rate limited. When you exceed a limit you get:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 42
```
```json
{ "error": { "code": "rate_limited", "message": "Too many requests. Please try again later." } }
```

`Retry-After` is the number of seconds to wait. Clients should back off and show a friendly
message rather than retrying immediately.

| Endpoint | Limit (per client IP) |
|---|---|
| `POST /auth/register` | burst 5, then ~30/hour |
| `POST /auth/login` | burst 10, then ~10/min |
| `POST /auth/login` (same IP **and** same email) | burst 5, then ~20/hour; cleared by a successful login |
| `POST /auth/refresh`, `POST /auth/logout` | burst 30, then ~30/min (shared) |
| `POST /auth/forgot-password` (per IP) | burst 5, then ~10/hour |
| `POST /auth/forgot-password` (per email address, any IP) | burst 3, then ~3/hour; when exceeded the request still answers `202` but no email is sent |
| `POST /auth/reset-password` | burst 10, then ~2/min |
| `POST /auth/verify-email` | burst 10, then ~2/min |
| `POST /auth/resend-verification` (per IP) | burst 5, then ~10/hour |
| `POST /auth/resend-verification` (per signed-in user) | burst 3, then ~3/hour |
| `POST /auth/change-password` (per IP) | burst 10, then ~10/min |
| `POST /auth/change-password` (per signed-in user) | burst 5, then ~20/hour |

`/health` and `GET /me` are not limited. The per-account login limit is keyed on IP + email, so an
attacker cannot lock a real user out of their account from a different address. Limits are enforced
per server process (in-memory) for now.

## CORS

Browser apps on an allow-listed origin (`FRONTEND_URL` on the server) may call the API.
Allowed: methods `GET, POST, OPTIONS`; request headers `Authorization, Content-Type`;
`X-Request-ID` and `Retry-After` are readable from JavaScript. Credentialed CORS (cookies) is not
enabled — authenticate with the `Authorization: Bearer` header. Preflight responses are cached for 10 minutes.
A preflight from an unlisted origin returns `403 cors_origin_not_allowed`.

## Token model

- **Access token**: JWT (HS256), 15 min by default. Send as `Authorization: Bearer <token>`.
- **Refresh token**: opaque random string, 30 days by default, **single use**. Each `/auth/refresh`
  returns a *new* refresh token; the old one stops working. Always store the newest one.
  Presenting an already-used refresh token revokes that entire login session.
- Serialize refresh calls in your client (don't fire two at once with the same token).

Token response shape (used by register, login, refresh):

```json
{
  "access_token": "eyJ...",
  "refresh_token": "kQ3...",
  "token_type": "Bearer",
  "expires_in": 900
}
```

---

## GET /health

Checks the server and database connectivity. No auth.

**200**
```json
{ "status": "ok", "database": "ok" }
```
**503**
```json
{ "status": "degraded", "database": "unavailable" }
```

---

## POST /api/v1/auth/register

Create an account and sign in.

**Request**
```json
{ "full_name": "Feranmi Oresajo", "email": "fer@example.com", "password": "a-long-password" }
```

| Field | Rules |
|---|---|
| `full_name` | required, trimmed, ≤ 100 chars, no control characters |
| `email` | required, valid address, ≤ 254 chars; trimmed and lowercased before storing |
| `password` | 10–128 characters |

Username is generated from `full_name` (`feranmi-oresajo`); on collision a numeric suffix is added
(`feranmi-oresajo-4821`).

**201 Created**
```json
{
  "user": {
    "id": "6f0c1f52-3a52-4a8e-9f3e-3b0f5a3d9c11",
    "full_name": "Feranmi Oresajo",
    "username": "feranmi-oresajo",
    "email": "fer@example.com",
    "email_verified": false,
    "created_at": "2026-09-19T21:50:00Z"
  },
  "access_token": "eyJ...",
  "refresh_token": "kQ3...",
  "token_type": "Bearer",
  "expires_in": 900
}
```

**Errors:** 400, 409 `email_taken`, 413, 422 `validation_failed`, 429.

```bash
curl -X POST localhost:8080/api/v1/auth/register -H 'Content-Type: application/json' \
  -d '{"full_name":"Feranmi Oresajo","email":"fer@example.com","password":"a-long-password"}'
```

---

## POST /api/v1/auth/login

**Request**
```json
{ "email": "fer@example.com", "password": "a-long-password" }
```

**200 OK** — same body as register (`user` + tokens). Each login creates a new independent
session (one per device).

**Errors:** 400, 401 `invalid_credentials`, 413, 429.

---

## POST /api/v1/auth/refresh

Exchange a refresh token for a new access token **and** a new refresh token.

**Request**
```json
{ "refresh_token": "kQ3..." }
```

**200 OK**
```json
{ "access_token": "eyJ...", "refresh_token": "new-token...", "token_type": "Bearer", "expires_in": 900 }
```

**Errors:** 400, 401 `invalid_refresh_token` → the client must send the user back to login; 429.

---

## POST /api/v1/auth/logout

Revokes the login session that the refresh token belongs to. Idempotent: always returns 204,
even for unknown/expired tokens. The current access token stays valid until it expires (≤ 15 min).

**Request**
```json
{ "refresh_token": "kQ3..." }
```

**204 No Content**

---

## POST /api/v1/auth/forgot-password

Starts a password reset by emailing a single-use link. **Always** answers `202` for any well-formed
email, whether or not an account exists, so it cannot be used to discover registered addresses.

**Request**
```json
{ "email": "fer@example.com" }
```

**202 Accepted**
```json
{ "message": "If an account exists for that email, a password reset link has been sent." }
```

**Errors:** 400, 413, 422 `validation_failed` (malformed email only), 429 (per-IP limit).

Notes:
- The link is `PASSWORD_RESET_URL#token=<token>` (URL **fragment**, so it is never sent to any server
  or written to access logs). It expires after 30 minutes (configurable) and works once.
- Requesting a new link cancels the previous one.
- Deactivated accounts receive nothing.
- Emails are sent in the background; delivery problems never change the response.

---

## POST /api/v1/auth/reset-password

Sets a new password using the token from the emailed link.

**Request**
```json
{ "token": "the-token-from-the-link", "new_password": "a-new-long-password" }
```

`new_password` follows the same rules as registration (10 to 128 characters).

**204 No Content**

On success, **all of the user's sessions are revoked** (every device is signed out) and the
email address is marked verified. A "your password was changed" notice is emailed. The user must
sign in again with the new password; this endpoint does not sign them in.

**Errors:**
- `400 invalid_reset_token`: unknown, expired, already used, or superseded. Send the user back to
  "forgot password".
- `422 validation_failed` with `fields.new_password`: password too short/long. **The link is not
  consumed**, so the user can retry with a stronger password.
- 429, 413, 400 `invalid_json`.

### Frontend reset page

1. Read the token from `location.hash` (`#token=...`) and immediately remove it from the address bar
   (`history.replaceState(null, "", location.pathname)`), so it doesn't linger in history or screenshots.
2. Show a "new password" form; `POST` `{ token, new_password }`.
3. On `204`, redirect to login with a "password updated" message. On `400 invalid_reset_token`, link to
   "request a new link". On `422`, show the field error and keep the form.

---

## POST /api/v1/auth/verify-email

Verifies the email address using the single-use token from the registration or resend email. The
token is in the URL fragment (`#token=`), so it is not sent to the API or included in HTTP logs.

**Request**
```json
{ "token": "the-token-from-the-link" }
```

**204 No Content**

The token is consumed atomically with marking the account's email verified. A replaced, expired,
used, unknown, or no-longer-valid link returns `400 invalid_verification_token`. Verification links
expire after 24 hours by default; requesting a replacement invalidates the previous link.

**Errors:** 400 `invalid_verification_token`, 413, 429.

---

## POST /api/v1/auth/resend-verification

Requests a fresh verification link for the signed-in account. Requires
`Authorization: Bearer <access_token>`.

**Request**
```json
{}
```

**202 Accepted** — a verification email was queued.
```json
{ "message": "Verification email sent." }
```

**200 OK** — the address is already verified; no email is sent.
```json
{ "message": "Your email is already verified." }
```

A new request invalidates any earlier unused verification link. Delivery is asynchronous, so a mail
queue failure does not change the response.

**Errors:** 401 `unauthorized`, 413, 429 (per IP or signed-in user).

---

## POST /api/v1/auth/change-password

Changes the password for the signed-in account. Requires
`Authorization: Bearer <access_token>`.

**Request**
```json
{ "current_password": "a-long-password", "new_password": "a-new-long-password" }
```

`new_password` follows the registration rules (10 to 128 characters) and must differ from the
current password.

**200 OK** — same token response as login. The current device remains signed in with the returned
tokens. All existing refresh sessions are revoked, pending password-reset links are deleted, and a
password-changed notice is emailed. Previously issued access tokens are stateless and remain valid
until they expire (15 minutes by default).

**Errors:** 400 `password_not_set`, 401 `unauthorized`, 409 `conflict` if another password change
wins the compare-and-swap, 413, 422 `validation_failed` (including incorrect `current_password`),
429 (per IP or signed-in user).

---

## GET /api/v1/me

Returns the authenticated user. Requires `Authorization: Bearer <access_token>`.

**200 OK**
```json
{
  "user": {
    "id": "6f0c1f52-3a52-4a8e-9f3e-3b0f5a3d9c11",
    "full_name": "Feranmi Oresajo",
    "username": "feranmi-oresajo",
    "email": "fer@example.com",
    "email_verified": false,
    "created_at": "2026-09-19T21:50:00Z"
  }
}
```

**401** `unauthorized` (response includes `WWW-Authenticate: Bearer realm="migo"`).

```bash
curl localhost:8080/api/v1/me -H "Authorization: Bearer $ACCESS_TOKEN"
```

---

## Recommended client flow

1. Register/login → store `access_token` (memory) and `refresh_token` (secure storage).
2. Call APIs with the access token.
3. On a `401 unauthorized`, call `/auth/refresh` **once**, replace both tokens, retry the request.
4. If refresh returns `401 invalid_refresh_token`, clear tokens and show the login screen.
5. On sign-out, call `/auth/logout` with the refresh token, then discard both tokens.

## Not yet implemented

Distributed (multi-instance) rate limiting, email verification
(sign-up), change-password for signed-in users, Google OAuth, session listing/management, and all Twilio/Paystack/rental domains.
