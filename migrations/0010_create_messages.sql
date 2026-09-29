CREATE TABLE messages (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                 uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    rental_id               uuid        NOT NULL,
    provider_number_id      uuid        NOT NULL,
    provider_config_id      uuid        NOT NULL,
    provider_kind           text        NOT NULL DEFAULT 'telephony',
    direction               text        NOT NULL,
    sender                  text        NOT NULL,
    recipient               text        NOT NULL,
    body                    text        NOT NULL,
    provider_message_id     text        NOT NULL,
    status                  text        NOT NULL,
    received_at             timestamptz,
    sent_at                 timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT messages_direction_check CHECK (direction IN ('inbound', 'outbound')),
    CONSTRAINT messages_status_check CHECK (status IN ('received', 'queued', 'sent', 'delivered', 'failed', 'rejected')),
    CONSTRAINT messages_sender_check CHECK (btrim(sender) <> ''),
    CONSTRAINT messages_recipient_check CHECK (btrim(recipient) <> ''),
    CONSTRAINT messages_provider_message_check CHECK (btrim(provider_message_id) <> ''),
    CONSTRAINT messages_time_check CHECK (
        (direction = 'inbound' AND received_at IS NOT NULL) OR
        (direction = 'outbound' AND sent_at IS NOT NULL)
    ),
    CONSTRAINT messages_rental_number_fkey FOREIGN KEY (rental_id, provider_number_id)
        REFERENCES rentals (id, provider_number_id) ON DELETE RESTRICT,
    CONSTRAINT messages_config_kind_fkey FOREIGN KEY (provider_config_id, provider_kind)
        REFERENCES provider_configs (id, provider_kind) ON DELETE RESTRICT,
    CONSTRAINT messages_provider_id_key UNIQUE (provider_config_id, direction, provider_message_id)
);

CREATE INDEX messages_user_created_idx ON messages (user_id, created_at DESC, id DESC);
CREATE INDEX messages_rental_created_idx ON messages (rental_id, created_at DESC, id DESC);

CREATE TABLE message_events (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id          uuid        NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
    event_type          text        NOT NULL,
    provider_payload    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT message_events_type_check CHECK (btrim(event_type) <> ''),
    CONSTRAINT message_events_payload_check CHECK (jsonb_typeof(provider_payload) = 'object')
);

CREATE INDEX message_events_message_created_idx ON message_events (message_id, created_at);

