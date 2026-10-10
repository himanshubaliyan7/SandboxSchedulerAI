-- 0003_webhooks: lifecycle webhooks. The events table is the outbox;
-- nothing here adds a write to the path that produces events.

-- The transaction that wrote each event. A webhook cursor cannot be a
-- plain event id: ids come from a sequence, so a transaction that took id
-- 10 may commit after one that took id 11, and a reader that had already
-- moved past 11 would never see 10. Ordered by (txid, id) and read only
-- below the oldest transaction still running
-- (pg_snapshot_xmin(pg_current_snapshot())), the log has no such holes:
-- everything below that horizon is final, everything written later sorts
-- above it. The column fills itself in; no INSERT names it. The default is
-- a stable function, so existing rows get this migration's transaction id
-- without a table rewrite.
ALTER TABLE events ADD COLUMN txid bigint NOT NULL DEFAULT (pg_current_xact_id()::text::bigint);
CREATE INDEX events_tenant_txid_idx ON events (tenant_id, txid, id) WHERE tenant_id IS NOT NULL;

-- One row per endpoint: its configuration, and the state of its delivery
-- (the cursor into the event log, the retry state and the lease of the
-- worker that is delivering to it right now).
CREATE TABLE webhooks (
    id          text PRIMARY KEY,
    tenant_id   text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    url         text NOT NULL,
    -- The signing secret, in the clear: unlike an API key, which is only
    -- ever compared, the secret is an HMAC key the server must use.
    secret      text NOT NULL,
    -- After a rotation the old secret keeps signing (next to the new one)
    -- until previous_secret_expires_at.
    previous_secret            text NOT NULL DEFAULT '',
    previous_secret_expires_at timestamptz,
    -- Public event types ("sandbox.created", ...) or the single entry "*".
    event_types text[] NOT NULL,
    description text NOT NULL DEFAULT '',
    enabled     boolean NOT NULL DEFAULT true,
    -- Why enabled is false: 'user', 'failing' (auto-disabled) or
    -- 'endpoint_gone' (the endpoint answered 410).
    disabled_reason text NOT NULL DEFAULT '',
    disabled_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- The last event dealt with (delivered, filtered out or dropped), as
    -- (events.txid, events.id).
    cursor_txid     bigint NOT NULL,
    cursor_event_id bigint NOT NULL DEFAULT 0,
    -- Failed attempts on the event after the cursor.
    attempts        integer NOT NULL DEFAULT 0,
    -- Start of the current run of failures; NULL after a success.
    failing_since   timestamptz,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz,
    last_failure_at timestamptz,
    last_error      text NOT NULL DEFAULT '',
    dropped_events  bigint NOT NULL DEFAULT 0,
    -- A worker owns the webhook until lease_until; every completion is
    -- fenced by lease_token, which each claim and each configuration
    -- change increments.
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until timestamptz
);
CREATE INDEX webhooks_tenant_idx ON webhooks (tenant_id, created_at);
CREATE INDEX webhooks_due_idx ON webhooks (next_attempt_at) WHERE enabled;

-- The attempt log, for debugging and for recording what was given up on.
-- It is pruned by age; the cursor above, not this table, is what makes
-- delivery reliable.
CREATE TABLE webhook_deliveries (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    webhook_id  text NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    -- events.id; NULL for a test event. For a 'dropped' row that stands
    -- for a run of expired events, the last of them.
    event_id    bigint,
    -- The id sent as Webhook-Id ("evt_..."); '' on a gap row.
    message_id  text NOT NULL DEFAULT '',
    event_type  text NOT NULL DEFAULT '',
    attempt     integer NOT NULL DEFAULT 1,
    status      text NOT NULL CHECK (status IN ('succeeded', 'failed', 'dropped')),
    response_status integer NOT NULL DEFAULT 0,
    duration_ms integer NOT NULL DEFAULT 0,
    error_code  text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    -- How many events a 'dropped' row accounts for.
    dropped_events integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, id DESC);
CREATE INDEX webhook_deliveries_created_idx ON webhook_deliveries (created_at);
