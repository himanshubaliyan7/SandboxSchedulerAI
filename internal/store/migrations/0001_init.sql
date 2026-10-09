-- 0001_init: tenants, API keys, templates, sandboxes and the event log.
-- Each migration file runs inside one transaction.

CREATE TABLE tenants (
    id             text PRIMARY KEY,
    name           text NOT NULL,
    max_concurrent integer NOT NULL CHECK (max_concurrent >= 0),
    rate_per_sec   double precision NOT NULL CHECK (rate_per_sec >= 0),
    burst          integer NOT NULL CHECK (burst >= 0),
    weight         integer NOT NULL DEFAULT 1 CHECK (weight >= 1),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Only a SHA-256 hash of the key is stored. prefix is the short,
-- non-secret part of the key used to find the candidate row.
CREATE TABLE api_keys (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    prefix     text NOT NULL,
    key_hash   bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX api_keys_prefix_idx ON api_keys (prefix) WHERE revoked_at IS NULL;
CREATE INDEX api_keys_tenant_idx ON api_keys (tenant_id);

-- Every field of core.Template. Durations are whole milliseconds.
CREATE TABLE templates (
    id              text PRIMARY KEY,
    version         bigint NOT NULL DEFAULT 1,
    image           text NOT NULL,
    runtime         text NOT NULL DEFAULT 'crun',
    cpus            double precision NOT NULL,
    memory_bytes    bigint NOT NULL,
    pids_limit      bigint NOT NULL,
    workdir_bytes   bigint NOT NULL,
    tmp_bytes       bigint NOT NULL,
    nofile_limit    bigint NOT NULL,
    network         text NOT NULL DEFAULT 'none' CHECK (network IN ('none', 'allowlist', 'full')),
    idle_timeout_ms bigint NOT NULL DEFAULT 0 CHECK (idle_timeout_ms >= 0),
    max_lifetime_ms bigint NOT NULL DEFAULT 0 CHECK (max_lifetime_ms >= 0),
    warm_ttl_ms     bigint NOT NULL DEFAULT 0 CHECK (warm_ttl_ms >= 0),
    warm_target     integer NOT NULL DEFAULT 0 CHECK (warm_target >= 0),
    max_total       integer NOT NULL DEFAULT 0 CHECK (max_total >= 0),
    refill_rate     integer NOT NULL DEFAULT 1 CHECK (refill_rate >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Every field of core.Sandbox, plus the per-sandbox access-token hash and
-- the network policy chosen at claim time. template_id and tenant_id carry
-- no foreign key on purpose: DESTROYED rows are kept for audit and must
-- not block deleting a template or a tenant.
CREATE TABLE sandboxes (
    id                text PRIMARY KEY,
    template_id       text NOT NULL,
    template_version  bigint NOT NULL,
    host_id           text NOT NULL,
    state             text NOT NULL CHECK (state IN (
                          'CREATING', 'WARM', 'ASSIGNED', 'RUNNING', 'PAUSED',
                          'RECYCLING', 'DESTROYED', 'FAILED', 'UNKNOWN')),
    tenant_id         text,
    generation        bigint NOT NULL DEFAULT 0,
    instance_id       text NOT NULL DEFAULT '',
    reason            text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    assigned_at       timestamptz,
    last_activity_at  timestamptz,
    expires_at        timestamptz,
    deadline_at       timestamptz,
    access_token_hash bytea,
    network_policy    jsonb,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- A sandbox is owned by a tenant exactly when it has been assigned.
    CHECK (state NOT IN ('ASSIGNED', 'RUNNING', 'PAUSED') OR tenant_id IS NOT NULL),
    CHECK (state NOT IN ('CREATING', 'WARM') OR tenant_id IS NULL)
);

-- The claim: oldest WARM row of a template. host_id and template_version
-- are included so the claim's subquery is answered from the index alone.
CREATE INDEX sandboxes_warm_idx ON sandboxes (template_id, created_at)
    INCLUDE (host_id, template_version) WHERE state = 'WARM';
-- Sweep: transient states past their deadline.
CREATE INDEX sandboxes_deadline_idx ON sandboxes (host_id, deadline_at)
    WHERE deadline_at IS NOT NULL;
-- Sweep: max lifetime.
CREATE INDEX sandboxes_expires_idx ON sandboxes (host_id, expires_at)
    WHERE state IN ('RUNNING', 'PAUSED');
-- Sweep: idle timeout.
CREATE INDEX sandboxes_idle_idx ON sandboxes (host_id, last_activity_at)
    INCLUDE (template_id) WHERE state = 'RUNNING';
-- Reconciler listing and per-template counts: everything still holding host resources.
CREATE INDEX sandboxes_live_idx ON sandboxes (host_id, template_id, state)
    WHERE state <> 'DESTROYED';
-- Tenant quota count and per-tenant listing.
CREATE INDEX sandboxes_tenant_live_idx ON sandboxes (tenant_id)
    WHERE state IN ('ASSIGNED', 'RUNNING', 'PAUSED');
-- Garbage collection of audit rows.
CREATE INDEX sandboxes_destroyed_idx ON sandboxes (updated_at) WHERE state = 'DESTROYED';

-- Append-only log of every state change. It is both the audit trail and
-- the metering source (claimed / destroyed pairs per tenant).
CREATE TABLE events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sandbox_id text NOT NULL,
    tenant_id  text,
    type       text NOT NULL,
    from_state text NOT NULL DEFAULT '',
    to_state   text NOT NULL DEFAULT '',
    reason     text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL DEFAULT now(),
    data       jsonb
);
CREATE INDEX events_sandbox_idx ON events (sandbox_id, id);
CREATE INDEX events_tenant_idx ON events (tenant_id, at) WHERE tenant_id IS NOT NULL;
