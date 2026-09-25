CREATE TABLE users (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name      text        NOT NULL,
    username       text        NOT NULL,
    email          text        NOT NULL,
    -- NULL allows OAuth-only accounts later.
    password_hash  text,
    email_verified boolean     NOT NULL DEFAULT false,
    is_active      boolean     NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    -- Constraint names are referenced by the repository to map errors.
    CONSTRAINT users_email_key           UNIQUE (email),
    CONSTRAINT users_username_key        UNIQUE (username),
    CONSTRAINT users_email_lowercase     CHECK (email = lower(email)),
    CONSTRAINT users_username_format     CHECK (username ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);
