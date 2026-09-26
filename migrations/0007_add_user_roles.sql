ALTER TABLE users
    ADD COLUMN role text NOT NULL DEFAULT 'user';

ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'admin'));

CREATE INDEX users_role_idx ON users (role) WHERE role = 'admin';
