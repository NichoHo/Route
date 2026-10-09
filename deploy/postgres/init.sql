CREATE EXTENSION IF NOT EXISTS postgis;

CREATE TABLE IF NOT EXISTS courier_positions (
    courier_id TEXT NOT NULL,
    ts TIMESTAMPTZ NOT NULL,
    geom geometry(Point, 4326) NOT NULL,
    heading NUMERIC NOT NULL,
    seq BIGINT NOT NULL
) PARTITION BY RANGE (ts);

CREATE TABLE IF NOT EXISTS courier_positions_default PARTITION OF courier_positions DEFAULT;
CREATE INDEX IF NOT EXISTS idx_courier_positions_courier_ts ON courier_positions (courier_id, ts DESC);

CREATE TABLE IF NOT EXISTS orders (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT UNIQUE NOT NULL,
    pickup geometry(Point, 4326) NOT NULL,
    dropoff geometry(Point, 4326) NOT NULL,
    pickup_h3 TEXT NOT NULL,
    -- Keep in sync with internal/config.MaxPriority.
    priority INT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 9),
    status TEXT NOT NULL CHECK (status IN (
        'queued', 'batched', 'offered', 'assigned', 'exhausted', 'completed', 'cancelled'
    )),
    -- Keep in sync with internal/config.MaxAttempts.
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts <= 3),
    excluded_couriers TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    assigned_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ
);

-- The sweep re-triggers orders stuck in queued/batched, and separately gives
-- up on ones that have been unmatched for too long. Both queries filter on
-- status IN ('queued', 'batched') first, so partial indexes on just the
-- time column they each range over are enough and stay tiny (only ever as
-- many rows as are actually in flight).
CREATE INDEX IF NOT EXISTS idx_orders_stuck ON orders (updated_at) WHERE status IN ('queued', 'batched');
CREATE INDEX IF NOT EXISTS idx_orders_stale ON orders (created_at) WHERE status IN ('queued', 'batched');
-- The dashboard broadcast loop polls recent non-terminal orders.
CREATE INDEX IF NOT EXISTS idx_orders_active_recent ON orders (created_at) WHERE status NOT IN ('completed', 'exhausted', 'cancelled');

CREATE TABLE IF NOT EXISTS offers (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders(id),
    courier_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'accepted', 'declined', 'expired')),
    cost NUMERIC NOT NULL,
    -- Courier-to-pickup distance at offer time, for the load tests' pickup-distance numbers.
    -- Existing databases: ALTER TABLE offers ADD COLUMN pickup_m NUMERIC;
    pickup_m NUMERIC,
    deadline_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    UNIQUE (order_id, courier_id)
);

-- The matcher reads each candidate courier's recent resolved offers to score
-- their accept rate. (Performance only; existing databases can apply it by hand.)
CREATE INDEX IF NOT EXISTS idx_offers_courier_resolved ON offers (courier_id, resolved_at) WHERE state <> 'pending';
-- The sweep expires pending offers past their deadline.
CREATE INDEX IF NOT EXISTS idx_offers_pending_deadline ON offers (deadline_at) WHERE state = 'pending';
-- Database backstop for "one non-terminal offer per courier", behind the Redis
-- claim. If Redis loses its claims (restart, flush), this still stops a second
-- pending offer. Existing databases need it applied by hand:
--   CREATE UNIQUE INDEX offers_pending_courier_idx ON offers (courier_id) WHERE state = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS offers_pending_courier_idx ON offers (courier_id) WHERE state = 'pending';
-- The dashboard broadcast loop reads pending offers to draw them.
CREATE INDEX IF NOT EXISTS idx_offers_pending ON offers (order_id, courier_id) WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS assignments (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL UNIQUE REFERENCES orders(id),
    courier_id TEXT NOT NULL,
    offer_id TEXT NOT NULL UNIQUE REFERENCES offers(id),
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

-- Partial unique index as database-level guarantee of one active assignment per courier
CREATE UNIQUE INDEX IF NOT EXISTS assignments_active_courier_idx ON assignments(courier_id) WHERE completed_at IS NULL;

CREATE TABLE IF NOT EXISTS batches (
    id TEXT PRIMARY KEY,
    window_open_at TIMESTAMPTZ NOT NULL,
    window_close_at TIMESTAMPTZ NOT NULL,
    order_count INT NOT NULL,
    courier_count INT NOT NULL,
    solver TEXT NOT NULL CHECK (solver IN ('hungarian', 'greedy')),
    total_cost_batched NUMERIC,
    total_cost_greedy NUMERIC,
    -- Pickup distance of the same two solutions: the permanent batched-vs-greedy
    -- comparison (ROUTE_SPEC.md commitment 5). Existing databases:
    --   ALTER TABLE batches ADD COLUMN total_distance_batched NUMERIC, ADD COLUMN total_distance_greedy NUMERIC;
    total_distance_batched NUMERIC,
    total_distance_greedy NUMERIC,
    solve_duration_ms INT
);
CREATE INDEX IF NOT EXISTS idx_batches_window_close ON batches (window_close_at DESC);

CREATE TABLE IF NOT EXISTS surge_snapshots (
    h3 TEXT NOT NULL,
    window_end_at TIMESTAMPTZ NOT NULL,
    demand INT NOT NULL,
    supply INT NOT NULL,
    multiplier NUMERIC NOT NULL,
    PRIMARY KEY (h3, window_end_at)
);

CREATE TABLE IF NOT EXISTS outbox (
    id TEXT PRIMARY KEY,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Every row in this table is unpublished by definition: the relay deletes a
-- row as soon as it's produced to Redpanda, so there's no published_at to
-- index on, just the relay's own FIFO order.
CREATE INDEX IF NOT EXISTS idx_outbox_created_at ON outbox (created_at);
