CREATE TABLE outbox_events (
    event_id        UUID NOT NULL CONSTRAINT outbox_events_event_id_pk PRIMARY KEY,
    aggregate_type  TEXT NOT NULL,
    aggregate_id    UUID NOT NULL,
    event_type      TEXT NOT NULL,
    event_version   INT NOT NULL,
    payload         JSONB NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until    TIMESTAMPTZ,
    published_at    TIMESTAMPTZ,
    last_error      TEXT
);

-- Partial and ordered for exactly the publisher's own claim query (spec,
-- "Outbox e eventos"): "registros com publishedAt IS NULL e nextAttemptAt
-- <= now(), FOR UPDATE SKIP LOCKED em ordem de occurredAt". Once an event
-- is published it drops out of this index entirely, so it stays small
-- forever instead of growing with the whole event history.
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, occurred_at)
    WHERE published_at IS NULL;

GRANT SELECT, INSERT, UPDATE ON outbox_events TO wallet_app;

-- attempts, next_attempt_at, locked_until, published_at and last_error all
-- change legitimately as the publisher claims, sends and confirms an
-- event. payload, event_type and event_id must never: they are the fact
-- being published, and a republish after a crash (spec: "outra instância
-- republica com o mesmo eventId e o mesmo payload") depends on all three
-- never drifting from what was committed alongside the financial state.
CREATE FUNCTION outbox_events_block_immutable_columns() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.event_id <> OLD.event_id
        OR NEW.event_type <> OLD.event_type
        OR NEW.payload IS DISTINCT FROM OLD.payload THEN
        RAISE EXCEPTION 'outbox_events: event_id, event_type and payload are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_events_block_immutable_columns
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_block_immutable_columns();
