CREATE TABLE inbox_messages (
    id            UUID NOT NULL CONSTRAINT inbox_messages_id_pk PRIMARY KEY,
    consumer_name TEXT NOT NULL,
    message_id    TEXT NOT NULL,
    payload_hash  TEXT NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ,

    -- consumer_name and message_id are opaque identifiers the consumer
    -- does not control the shape of (an SQS MessageId, a hand-picked
    -- consumer label) - bounded the same way every other opaque id in this
    -- schema is: non-empty, at most 255 bytes.
    CONSTRAINT inbox_messages_consumer_name_len_check CHECK (octet_length(consumer_name) BETWEEN 1 AND 255),
    CONSTRAINT inbox_messages_message_id_len_check CHECK (octet_length(message_id) BETWEEN 1 AND 255),

    CONSTRAINT inbox_messages_consumer_name_message_id_key UNIQUE (consumer_name, message_id)
);

-- UNIQUE (consumer_name, message_id) is the whole idempotency guarantee for
-- SQS redelivery: the consumer's own INSERT ... ON CONFLICT DO NOTHING
-- (spec, "Inbox e consumidor SQS") depends on this constraint existing,
-- not on any check the application does first. UPDATE is granted only to
-- let the consumer set completed_at once the shared use case commits.
GRANT SELECT, INSERT, UPDATE ON inbox_messages TO wallet_app;
