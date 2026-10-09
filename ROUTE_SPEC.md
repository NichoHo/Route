# Route — Blueprint

**Status:** greenfield. This is the project's blueprint, not a migration document.
It becomes the repository's `BLUEPRINT.md` on day one.

**Relationship to Agora:** independent. Route is a separate repository solving a
different problem class (geospatial matching under throughput). An optional
one-way integration is described in Section 14 and is not required for Route to
be complete.

---

## Table of contents

1. [What it is](#1-what-it-is)
2. [Design commitments](#2-design-commitments)
3. [Scope and non-goals](#3-scope-and-non-goals)
4. [Architecture](#4-architecture)
5. [Domain model](#5-domain-model)
6. [Subsystems](#6-subsystems)
7. [Data model](#7-data-model)
8. [API surface](#8-api-surface)
9. [Cross-cutting concerns](#9-cross-cutting-concerns)
10. [Test strategy](#10-test-strategy)
11. [Load test plan](#11-load-test-plan)
12. [Architecture decision records](#12-architecture-decision-records)
13. [Phases](#13-phases)
14. [Optional Agora integration](#14-optional-agora-integration)
15. [Security and hardening](#15-security-and-hardening)

---

## 1. What it is

Route is a real-time dispatch engine. Given a continuous stream of incoming
delivery orders and a population of moving couriers, it assigns each order to a
courier within seconds, at high throughput, without ever assigning one courier to
two orders at once, while balancing supply against demand across a city.

### The problem

Naive dispatch (nearest available courier, first come first served) produces poor
global outcomes: two nearby orders can pull two couriers across each other, a
courier can be offered an order and hold it while a better candidate sits idle,
and demand spikes in one district can starve it while adjacent districts have
slack. The engineering difficulty is not any single assignment. It is doing
thousands of them per minute, concurrently, correctly, while the inputs keep moving.

### What it does

- Ingests courier location pings at high volume and keeps a live spatial index.
- Collects incoming orders into short batching windows and solves each batch as
  an assignment problem rather than greedily one at a time.
- Runs an offer lifecycle per assignment (offer, accept, decline, timeout, re-offer)
  with idempotent acceptance so no courier is ever double-booked.
- Computes surge multipliers per zone from a sliding supply and demand window.
- Ships with a simulator that doubles as the demo and the load generator.
- Renders everything on a live map.

### Honest framing

Route is a simulation. Couriers are synthetic and move along a random walk with
momentum inside a bounding box, not on real roads. Distance is great-circle
(haversine), not road distance; a real ETA needs a routing engine such as OSRM or
Valhalla, which is out of scope and named as the upgrade path. Orders are
generated, not placed by people. No money moves.

---

## 2. Design commitments

1. **No courier is ever assigned two active orders.** Enforced by an atomic claim,
   tested under concurrency and induced failure.
2. **No order is ever lost.** Every order terminates in exactly one of: assigned,
   exhausted (no courier accepted after N attempts), or cancelled.
3. **Every state-changing message is idempotent.** A duplicate accept, decline, or
   ping has no second effect.
4. **Location pings are droppable; orders are not.** Under backpressure, pings are
   shed (they are last-write-wins state) and orders are queued. This asymmetry is
   the core of the backpressure policy.
5. **Batched matching must be measurably better than greedy** on the same input,
   or it is not worth its latency cost. The comparison is a permanent metric, not
   a one-time benchmark.
6. **Behaviour under load is measured and published**, not assumed.

---

## 3. Scope and non-goals

### In scope

Everything in Sections 4 through 13.

### Non-goals

Each of these is a plausible next step and each is excluded so the core stays sharp.

- **No real road network or routing engine.** Haversine distance. The ADR names
  OSRM as the upgrade path.
- **No real map tiles requiring an API key.** MapLibre GL with a keyless tile
  source. The demo must work for anyone who opens the URL.
- **No courier mobile app.** The simulator is the courier client.
- **No multi-instance matcher on day one.** One matcher process. The claim
  mechanism is designed to be multi-instance safe and is tested with concurrent
  goroutines, so splitting later is a deployment change, not a redesign.
- **No Kubernetes** unless single-instance Docker Compose fails the Section 11 targets.
- **No payment, no pricing beyond the surge multiplier, no customer-facing UI.**
- **No machine-learned ETA or demand forecasting.** Surge is a deterministic
  function of a sliding window. Forecasting is a named extension, not a phase.
- **No integration with Agora until both are independently complete.** Section 14.

---

## 4. Architecture

### 4.1 Topology

```
                     routesim (Go binary, separate process)
                     N simulated couriers + order generator
                              │ gRPC stream         │ HTTP
                              │ (location pings)    │ (create order)
                              ▼                     ▼
   ┌──────────────────────────────────────────────────────────────┐
   │                      routed (Go binary)                      │
   │                                                              │
   │   ingest ──► index ──► matcher ──► offers ──► assignments    │
   │                 │          │          │            │         │
   │                 │        surge        │            │         │
   │                 │          │          │            │         │
   │                 ▼          ▼          ▼            ▼         │
   │              Redis     Redis      Postgres     Postgres      │
   │             (GEO +    (surge     (orders,     (assignments,  │
   │              claims)   cache)     offers)      history)      │
   │                                                              │
   │                      transactional outbox                    │
   └──────────────────────────────┬───────────────────────────────┘
                                  │
                              Redpanda
                                  │
                    ┌─────────────┴──────────────┐
                    │                            │
              surge consumer              analytics / Agora
                                          (optional, §14)

   Next.js dashboard ◄── WebSocket ── routed (live couriers, orders, matches, surge)
```

### 4.2 Processes

| Process | Language | Responsibility |
|---|---|---|
| `routed` | Go | The server. Ingest, spatial index, matcher, offer lifecycle, surge, HTTP and gRPC API, WebSocket fan-out to the dashboard. One binary, internal components as goroutine-owned subsystems. |
| `routesim` | Go | The client. Simulates N couriers emitting pings and responding to offers, plus an order generator with configurable rate and spatial distribution. Also the load generator. |
| dashboard | Next.js / TS | Live map. Couriers, orders, assignments, surge heatmap, and a metrics strip. |

`routed` is deliberately a single process. Splitting `matcher` into its own service
is a Phase 6 option gated on load results, not a starting assumption.

### 4.3 Repository layout

```
route/
  cmd/
    routed/          server entrypoint
    routesim/        simulator entrypoint
  internal/
    ingest/          gRPC ping stream handler, backpressure, shedding
    index/           Redis GEO wrapper, staleness, k-nearest
    matcher/         batching window, cost matrix, Hungarian, greedy fallback
    offers/          offer state machine, timeouts, re-offer, idempotency
    claims/          atomic courier claim (Redis SET NX)
    surge/           H3 zoning, sliding window, multiplier
    sim/             courier movement model, order generator, behaviour model
    api/             HTTP handlers, WebSocket hub
    events/          outbox writer, relay, topic definitions
    geo/             haversine, H3 helpers, bounding box
  web/
    dashboard/       Next.js + MapLibre GL
  deploy/
    compose/
    terraform/
  loadtest/
    scenarios/       routesim profiles used as load scenarios
    results/         committed JSON + rendered charts
  docs/
    BLUEPRINT.md     this file
    decisions/       ADRs (Section 12)
  migrations/
```

### 4.4 Stack

| Concern | Choice | Why |
|---|---|---|
| Server language | Go | Concurrency model fits a matcher; consistent with Agora. |
| Live spatial index | Redis GEO (`GEOADD`, `GEOSEARCH`) | Fast k-nearest over thousands of points; no custom index to maintain. |
| Zoning | H3 (`uber/h3-go`) | Uniform hexagonal neighbours make surge and batching-by-zone simple. |
| Courier claims | Redis `SET NX PX` | Atomic, TTL-backed, multi-instance safe. |
| Durable store | PostgreSQL + PostGIS | Orders, offers, assignments, history, spatial queries over history. |
| Events | Redpanda (Kafka-compatible) via transactional outbox | Same pattern as Agora; no new mechanism. |
| Courier protocol | gRPC bidirectional stream | Pings up, offers down, one connection per courier. |
| Dashboard transport | WebSocket | Live fan-out of positions and matches. |
| Map | MapLibre GL + keyless tiles | No API key; demo works for anyone. |
| Metrics | Prometheus + Grafana | |
| Tracing | OpenTelemetry + Jaeger | One trace from order creation to offer sent. |
| Load | `routesim` profiles + k6 for the HTTP API | The simulator is the load generator. |

---

## 5. Domain model

### 5.1 Entities

**Courier.** An identity with a current position, a heading, a status, and a
last-seen timestamp. Synthetic in this project.

**Order.** A pickup point, a dropoff point, a creation time, a priority, and a
status. Enters the system via HTTP and is durable from the moment it is accepted.

**Batch.** A set of orders collected within one matching window, plus the candidate
couriers gathered for them. Ephemeral; exists for one solve.

**Offer.** A proposal of one order to one courier, with a deadline. The unit of
the offer lifecycle.

**Assignment.** An accepted offer. Durable. Exactly one per order once matched.

**Zone.** An H3 cell at a fixed resolution. The unit of surge computation and the
grouping key for candidate gathering.

### 5.2 Courier state machine

```
offline ──► available ──► offered ──► assigned ──► available
              ▲              │            │
              │              └── decline / timeout ──► available
              │                                            │
              └──────────────── stale (no ping > T) ◄──────┘
                                    │
                                    └──► offline
```

- `available`: pinging, no active offer or assignment. Eligible as a candidate.
- `offered`: holds exactly one pending offer. Claimed. Not eligible.
- `assigned`: holds exactly one assignment. Not eligible.
- `stale`: no ping within the staleness threshold. Excluded from candidates. Any
  pending offer is expired. Returns to `available` on next ping.
- `offline`: explicit disconnect or prolonged staleness.

### 5.3 Order state machine

```
created ──► queued ──► batched ──► offered ──► assigned ──► completed
                          ▲            │
                          │            └── declined / timed out
                          │                       │
                          └───── re-queue ◄───────┘  (attempts < N)
                                                  │
                                                  └──► exhausted  (attempts == N)

any pre-assigned state ──► cancelled
```

- `queued`: accepted, waiting for the next batch window.
- `batched`: inside a window being solved.
- `offered`: an offer is out to a courier.
- `assigned`: a courier accepted. Terminal for the matcher.
- `exhausted`: N attempts failed. Terminal. Surfaced on the dashboard.
- `completed`: simulated delivery finished. Terminal.
- `cancelled`: terminal, only from pre-assigned states.

### 5.4 Offer state machine

```
pending ──► accepted
   │
   ├──► declined
   │
   └──► expired   (deadline passed)
```

Terminal states are absorbing. An accept arriving after `expired` is rejected
with a specific error and has no effect. An accept arriving twice is idempotent.

---

## 6. Subsystems

### 6.1 Ingest

- Each courier holds one gRPC bidirectional stream: pings upstream, offers
  downstream.
- Ping rate: one every 3 seconds per courier by default (configurable in the simulator).
- Each ping carries `courier_id`, `lat`, `lng`, `heading`, `client_ts`, `seq`.
- Out-of-order pings (by `seq`) are dropped. Pings older than the staleness
  threshold on arrival are dropped.
- **Shedding under backpressure:** if the ingest channel is full, the oldest
  buffered ping for that courier is replaced by the newest. Pings are
  last-write-wins state; dropping an intermediate one loses nothing. A metric
  counts sheds so the behaviour is visible.

### 6.2 Spatial index

- Live courier positions in Redis GEO under a single key: `route:couriers`.
- A parallel key per courier, `route:courier:{id}:seen`, with a TTL equal to the
  staleness threshold. Candidate gathering filters out any courier whose `seen`
  key has expired.
- Candidate query: `GEOSEARCH` by radius from the order's pickup point, ordered
  by distance, limited to K. Radius expands in steps (1km, 2km, 4km) until at
  least `min_candidates` are found or the cap is hit.
- Durable position history is written to PostGIS asynchronously and is never on
  the matching path.

### 6.3 Matcher (the core)

**Batching window.** Orders are collected for `window_ms` (default 2000). At window
close, or when `max_batch_size` is reached, the batch is solved.

**Candidate gathering.** For each order in the batch, gather up to K nearest
available couriers. The union of candidates forms the batch's courier set.

**Cost matrix.** Rows are orders, columns are candidate couriers. Cost is the
scoring function in 6.4. Missing pairs (courier outside an order's search radius)
get an infinite cost.

**Solver.**

- If `orders × couriers` is at or below `hungarian_threshold` (default 200 × 200),
  solve with the Hungarian algorithm for the optimal assignment.
- Above the threshold, use greedy on the sorted cost list. The threshold is
  configurable and the choice is recorded per batch in metrics.

**Claiming.** For each proposed pair, attempt the atomic claim (6.6). A failed
claim (courier already claimed by a concurrent batch or a lingering offer) removes
that courier from the solution and the affected order is re-queued for the next
window with incremented priority.

**Comparison metric.** Every batch also computes what greedy would have produced
and records `total_distance_batched` versus `total_distance_greedy`. This runs
permanently, not only in benchmarks, so the dashboard can show the improvement
live. Commitment 5 depends on this.

### 6.4 Scoring

Cost for pairing order `o` with courier `c`:

```
cost = w_dist   * haversine(c.pos, o.pickup)
     + w_wait   * (now - o.created_at)              (negative weight: older orders are cheaper to assign)
     + w_accept * (1 - c.historical_accept_rate)
     + w_load   * zone_supply_demand_ratio(o.pickup) (nudges assignment away from over-served zones)
```

Weights are configuration. Defaults are documented with the reasoning in an ADR.
The function is pure and unit-tested in isolation.

### 6.5 Offer lifecycle

1. Matcher emits an assignment proposal. `offers` creates an `Offer` in `pending`
   with `deadline = now + offer_ttl` (default 15s) and pushes it down the courier's
   stream.
2. Courier responds `accept` or `decline`, or the deadline passes.
3. **Accept:** transition offer to `accepted`, create the `Assignment`, transition
   order to `assigned`, transition courier to `assigned`. All in one Postgres
   transaction, with an outbox row. The claim is converted from an offer claim to
   an assignment claim (same key, longer TTL).
4. **Decline or expiry:** transition offer to terminal, release the claim, add the
   courier to the order's `excluded_couriers` set, increment `attempts`. If
   `attempts < max_attempts` (default 3), re-queue the order with boosted priority.
   Otherwise mark `exhausted`.
5. **Idempotency:** accept and decline messages carry the `offer_id`. A message for
   an offer already in a terminal state returns the terminal state and changes
   nothing. Duplicate accepts for a `pending` offer race on a single-row
   `UPDATE ... WHERE state = 'pending'`; exactly one wins.

### 6.6 Claims

- Key: `route:claim:{courier_id}`. Value: the `offer_id` or `assignment_id`.
- Set with `SET key value NX PX ttl`. Success means the courier is claimed.
- Offer claims carry `offer_ttl + grace`. Assignment claims carry the expected
  delivery duration plus grace, and are renewed by the courier's pings while assigned.
- Released explicitly on decline, expiry, completion, and cancellation. TTL is the
  backstop for crashes.
- A courier whose claim exists is never a candidate, regardless of what the
  courier state in Postgres says. Redis is the authority for eligibility on the
  hot path; Postgres is the durable record. Reconciliation runs every 30s and
  alerts on disagreement.

### 6.7 Surge

- Zone resolution: H3 resolution 8 (roughly 0.7 km² cells) by default.
- Sliding window: 5 minutes, bucketed at 30s.
- Per zone: `demand` = orders created with pickup in the zone during the window;
  `supply` = distinct available couriers whose most recent ping was in the zone.
- Multiplier: a monotone step function of `demand / max(supply, 1)`, clamped to
  `[1.0, max_surge]` (default 3.0). The exact steps are configuration.
- Recomputed every 30s, written to `route:surge:{h3}` in Redis, and published as
  `surge.updated` to Redpanda.
- The dashboard renders surge as a hex heatmap over the map.

### 6.8 Simulator (`routesim`)

The simulator is not a test fixture. It is the demo, the load generator, and the
only courier client. Treat it as a first-class component.

**Courier movement.** Random walk with momentum inside a configurable bounding box
(default: a rectangle over central Jakarta). Speed and turn rate are per-courier
parameters drawn from a distribution. When assigned, a courier moves toward the
pickup, then the dropoff, at its speed; on arrival it reports completion.

**Courier behaviour.** Per-courier `accept_probability` (default distribution
centred on 0.85) and `response_delay` (default 1 to 8 seconds). Some fraction are
configured as "flaky" and stop pinging at random for a while, to exercise staleness.

**Order generation.** A Poisson process at a configurable rate. Pickup locations
are drawn from a mixture: uniform over the box plus N hotspots with Gaussian
spread, so surge has something to react to. Dropoffs are pickup plus a random
offset. Rate and hotspot weights can change over time via a scenario file, which
is how spike tests are expressed.

**Profiles.** A scenario file names courier count, order rate over time, hotspot
configuration, and behaviour parameters. Load test scenarios in Section 11 are
committed profiles.

### 6.9 Event backbone

Transactional outbox in Postgres, relayed to Redpanda. Same pattern as Agora.

| Topic | Emitted by | Payload |
|---|---|---|
| `order.created` | api | order id, pickup, dropoff, zone |
| `order.assigned` | offers | order id, courier id, batch id, cost |
| `order.exhausted` | offers | order id, attempts |
| `order.completed` | ingest (via courier report) | order id, courier id, duration |
| `offer.sent` | offers | offer id, order id, courier id, deadline |
| `offer.resolved` | offers | offer id, outcome |
| `batch.solved` | matcher | batch id, size, solver used, batched vs greedy distance |
| `surge.updated` | surge | zone, multiplier, demand, supply |

### 6.10 Dashboard

- MapLibre GL map with keyless tiles.
- Layers: couriers (coloured by state), open orders, active assignments (line from
  courier to pickup), surge hex heatmap.
- Metrics strip: orders per second, match latency p50/p99, match rate, active
  couriers, batched-versus-greedy improvement, shed pings per second, queue depth.
- Controls: start/stop a simulator profile, adjust order rate live.
- A single WebSocket from `routed` fans out deltas at a capped rate (default 4 Hz)
  regardless of internal event rate.

---

## 7. Data model

### 7.1 PostgreSQL

```sql
couriers
  id, display_name, status, last_seen_at, accept_rate, created_at

courier_positions          -- history only, never on the matching path
  courier_id, ts, geom (PostGIS point), heading, seq
  PARTITION BY RANGE (ts)

orders
  id, pickup (point), dropoff (point), pickup_h3, priority, status,
  attempts, excluded_couriers (uuid[]), created_at, updated_at,
  assigned_at NULL, completed_at NULL, cancelled_at NULL

batches
  id, window_open_at, window_close_at, order_count, courier_count,
  solver ('hungarian' | 'greedy'), total_cost_batched, total_cost_greedy,
  solve_duration_ms

offers
  id, order_id, courier_id, batch_id, state, cost, deadline_at,
  created_at, resolved_at NULL
  UNIQUE (order_id, courier_id, batch_id)

assignments
  id, order_id UNIQUE, courier_id, offer_id UNIQUE, accepted_at,
  completed_at NULL
  -- UNIQUE(order_id) is the database-level guarantee of one assignment per order

surge_snapshots
  h3, window_end_at, demand, supply, multiplier

outbox
  id, topic, payload, created_at, published_at NULL
```

Invariants enforced by the schema, not only by code:

- `assignments.order_id UNIQUE`: one assignment per order.
- A partial unique index on `assignments(courier_id) WHERE completed_at IS NULL`:
  one active assignment per courier, as a backstop behind the Redis claim.
- A CHECK on `orders.attempts <= max_attempts`.

### 7.2 Redis

```
route:couriers                    GEO set of live positions
route:courier:{id}:seen           string with TTL = staleness threshold
route:claim:{courier_id}          SET NX PX; value = offer_id | assignment_id
route:surge:{h3}                  current multiplier
route:queue                       list of order ids awaiting the next window
```

---

## 8. API surface

### 8.1 gRPC (courier protocol, used by `routesim`)

```
service Courier {
  rpc Session(stream CourierMessage) returns (stream ServerMessage);
}

CourierMessage  = Ping | OfferResponse | CompletionReport
ServerMessage   = Offer | OfferOutcome | Ack
```

One long-lived stream per courier. `seq` on every client message.

### 8.2 HTTP (orders and dashboard)

```
POST   /orders                     create an order (idempotency key required)
GET    /orders/{id}
POST   /orders/{id}/cancel
GET    /couriers                   paginated, filterable by status
GET    /batches?since=             recent batches with solver stats
GET    /surge                      current multipliers, all zones
GET    /metrics                    Prometheus
GET    /healthz
WS     /live                       dashboard delta stream
POST   /sim/profiles/{name}/start  dashboard control (local demo only)
POST   /sim/stop
```

`POST /orders` requires an `Idempotency-Key` header. A repeated key returns the
original order with status 200, creating nothing.

---

## 9. Cross-cutting concerns

### 9.1 Authentication

The public demo exposes read endpoints and the WebSocket. Mutating endpoints
(`POST /orders`, `/sim/*`) are gated by a bearer token in the demo deployment so
the public URL cannot be used as a free load generator against itself. Courier
gRPC sessions present a per-courier token issued at simulator start.

This is deliberately minimal. Section 15 covers what is and is not hardened.

### 9.2 Tracing

OpenTelemetry to Jaeger. One trace must follow:

```
POST /orders
  → queued
  → batch window close
  → candidate gathering (Redis GEOSEARCH)
  → solve (span tagged with solver and matrix size)
  → claim (Redis SET NX)
  → offer created (Postgres)
  → offer pushed on courier stream
  → offer accepted
  → assignment committed + outbox
```

A screenshot of that trace belongs at the top of the README.

### 9.3 Metrics

Prometheus, scraped from `routed`.

| Metric | Type | Why it matters |
|---|---|---|
| `route_match_latency_seconds` | histogram | Order created → offer sent. The headline number. |
| `route_match_rate` | gauge | Assigned / (assigned + exhausted), rolling. |
| `route_batch_size` | histogram | |
| `route_batch_solve_seconds` | histogram | Tagged by solver. |
| `route_batched_vs_greedy_ratio` | gauge | Commitment 5. |
| `route_queue_depth` | gauge | Backpressure indicator. |
| `route_pings_shed_total` | counter | Backpressure indicator. |
| `route_claim_conflicts_total` | counter | Concurrent batch contention. |
| `route_stale_couriers` | gauge | |
| `route_offer_outcome_total` | counter | Tagged accept / decline / expired. |

### 9.4 Deployment

- Docker Compose locally: `make up` brings up `routed`, Postgres, Redis, Redpanda,
  Prometheus, Grafana, Jaeger, and the dashboard. `make sim PROFILE=baseline`
  starts the simulator.
- Terraform to a single cloud instance for the public demo. Reuse Agora's
  single-instance module.
- The public demo runs a low-rate simulator profile continuously so the map is
  always alive when someone opens it.

---

## 10. Test strategy

Every item is a build-breaking test. Invariants run under the Go race detector.

### 10.1 Assignment invariants

- **No double booking.** Across N concurrent batch solves against a shared courier
  pool, no courier holds more than one non-terminal offer or active assignment at
  any instant. Verified by both the Redis claim and the Postgres partial unique index.
- **No lost orders.** Every created order terminates in exactly one of `assigned`,
  `exhausted`, `cancelled`. Property test over random scenarios.
- **Exactly-once accept.** 20 concurrent accepts for one pending offer produce
  exactly one assignment; 19 receive the terminal-state error.
- **Late accept is rejected.** An accept after `expired` changes nothing and
  returns a specific error.
- **Claim release on every non-accept path.** After decline, expiry, cancellation,
  and courier disconnect, the claim key does not exist.
- **Attempt cap.** An order never receives more than `max_attempts` offers.

### 10.2 Matching invariants

- **Batched ≤ greedy.** For any batch, total cost of the Hungarian solution is at
  most the total cost of greedy on the same matrix. Property test, 1,000 random
  matrices.
- **Excluded couriers are never re-offered** the same order.
- **Stale couriers are never candidates.** A courier whose `seen` key has expired
  does not appear in any batch.
- **Search radius expansion terminates** and respects the cap.

### 10.3 Surge invariants

- Multiplier is monotone non-decreasing in `demand / supply`.
- Multiplier is bounded in `[1.0, max_surge]`.
- A zone with zero demand has multiplier 1.0.

### 10.4 Ingest invariants

- Out-of-order pings (lower `seq`) are dropped and counted.
- Under a full channel, the newest ping wins and the shed counter increments.
- Position in Redis GEO always reflects the highest `seq` received.

### 10.5 Chaos

- **Kill Redis mid-batch.** Assert: the in-flight batch is abandoned, its orders
  re-queue, no assignment was created without a claim, no double booking after
  recovery. Claims that were set before the kill expire by TTL.
- **Kill `routed` mid-batch.** Same assertions. On restart, orders in `batched`
  state return to `queued`.
- **Courier disconnects while holding a pending offer.** Offer expires, claim
  releases, order re-queues with the courier excluded.
- **Simulated clock skew** between `routesim` and `routed` of ±30s. Staleness
  and offer deadlines are evaluated on server time only; assert nothing breaks.

### 10.6 End to end

One scripted scenario: 50 couriers, 20 orders over 30 seconds, one flaky courier,
one hotspot. Assert final state: every order terminal, every courier consistent
between Redis and Postgres, batched-versus-greedy ratio recorded for every batch,
dashboard WebSocket received every assignment.

---

## 11. Load test plan

Publish real numbers from a single instance. State the hardware in the README.
Modest and honest beats inflated. `routesim` profiles are the load generator;
k6 covers the HTTP API only.

| Scenario | Profile | Target |
|---|---|---|
| Baseline | 1,000 couriers, 50 orders/s, 10 min | Match latency p99 < 3s. Match rate > 98%. Zero sheds. |
| Throughput | 5,000 couriers, 500 orders/s, 5 min | Match latency p99 < 4s. Match rate > 95%. Queue depth stable. |
| Ping flood | 5,000 couriers at 1 ping/s (≈5,000 pings/s) | Index lag < 1s. Sheds acceptable; match latency unaffected. |
| Hotspot spike | 2,000 orders in 10s into one zone, 1,000 couriers | Batch cap holds. Queue drains within 60s. No double booking. Surge reaches max in that zone. |
| Contention | 200 couriers, 500 orders/s (deliberately under-supplied) | Exhausted rate is nonzero and reported honestly. No double booking. Claim conflicts counted. |
| Solver comparison | Baseline profile, alternating batch windows 0ms / 1000ms / 2000ms / 4000ms | Publish distance improvement versus added latency for each window. This is the chart that justifies batching. |

Results committed as JSON plus rendered charts under `loadtest/results/`, re-run
on a CI schedule so the numbers do not rot.

**If a target is missed, publish the miss and the reason.** A documented
bottleneck with a named fix is a better artifact than a passing number.

---

## 12. Architecture decision records

Write these as short ADRs under `docs/decisions/`. Each records the options
considered, the choice, and what would change the decision.

| # | Decision | Options considered |
|---|---|---|
| 001 | Redis GEO for the live index, H3 for zoning, PostGIS for history only | H3-only in memory; PostGIS on the hot path; a custom quadtree |
| 002 | Batched matching with a 2s window, Hungarian under a size threshold, greedy above | Greedy only; always Hungarian; min-cost flow |
| 003 | Courier claim via Redis `SET NX PX`, Postgres partial unique index as backstop | Postgres `SELECT FOR UPDATE` only; in-process lock (single instance) |
| 004 | Haversine distance, not road distance | OSRM sidecar; Valhalla; straight-line Euclidean |
| 005 | Pings are droppable, orders are not | Drop nothing (unbounded buffers); drop orders under load |
| 006 | Single `routed` process with multi-instance-safe claims | Split matcher on day one; split ingest on day one |
| 007 | Server time is authoritative for staleness and deadlines | Trust client timestamps; hybrid logical clocks |
| 008 | The simulator is a first-class component, not a test fixture | Replay recorded data; k6 only |

ADR 002 and 003 are the two most discussable decisions in the project. Write them
before the code they govern.

---

## 13. Phases

Ordered by value per hour. Each has an exit criterion. Do not reorder.

### Phase 0 — Skeleton and moving dots

`routed` starts, Postgres, Redis, Redpanda, and the dashboard come up via
`make up`. `routesim` streams pings for 50 couriers. The dashboard shows them moving.

**Exit:** dots move on a map from a single command.

### Phase 1 — Ingest and index

Pings land in Redis GEO. Staleness keys with TTL. `GEOSEARCH` k-nearest works
with radius expansion. Shedding under a full channel is implemented and counted.
Position history written to PostGIS asynchronously.

**Exit:** 10.4 invariants pass. A stale courier disappears from candidate queries.

### Phase 2 — Greedy matcher and offer lifecycle

Orders via HTTP with idempotency key. Greedy nearest-available matching (no
batching yet). Full offer state machine. Claims. Re-offer on decline and expiry.
Attempt cap. Simulator couriers accept and decline per their behaviour model.

**Exit:** 10.1 invariants pass under the race detector. The no-double-booking test
runs with 50 concurrent solvers against 100 couriers.

### Phase 3 — Batched matcher

Batching window. Cost matrix. Hungarian with greedy fallback above the threshold.
Permanent batched-versus-greedy comparison metric. Scoring function with
configurable weights.

**Exit:** 10.2 invariants pass. The solver comparison scenario from Section 11
produces a chart showing improvement versus window length.

### Phase 4 — Surge and events

H3 zoning. Sliding window. Multiplier. `surge.updated` on Redpanda. Hex heatmap
on the dashboard. All topics in 6.9 flowing through the outbox.

**Exit:** 10.3 invariants pass. The hotspot spike scenario drives a zone to max
surge visibly on the map.

### Phase 5 — Tracing, load, write-up

OpenTelemetry across the full path. Every scenario in Section 11 run and
committed. Chaos tests in 10.5 passing. One long-form engineering post on the
hardest problem encountered. Candidates: the claim-versus-concurrent-batch race,
the batching latency trade-off, or the Redis-death recovery.

**Exit:** a single Jaeger trace from `POST /orders` to `assignment committed`,
screenshotted. Load results committed. Post published. Public demo running the
baseline profile continuously.

### Phase 6 — Security and hardening

Section 15.

### If time runs short

Phases 0 through 3 finished and documented beats all seven half-built. Phase 3 is
where the project stops being a CRUD app with a map and starts being a matching
engine.

---

## 14. Optional Agora integration

Not required. Build only after both projects are independently complete.

**Shape:** Route runs one additional Redpanda consumer subscribed to Agora's
`order.confirmed` topic. On each event it creates a Route order with pickup at the
seller's location and dropoff at the buyer's. Route publishes `order.completed`
back; Agora's `market` service optionally consumes it to advance the marketplace
order to `shipped` → `completed`.

**Boundaries:** one topic in, one topic out. No shared database, no synchronous
calls, no shared code beyond the event schema. If the integration needs more than
that, it is scope creep and should not be built.

---

## 15. Security and hardening

**Tier: T1, Personal.** Reasoning: the demo is publicly reachable, but it holds
only synthetic couriers, generated orders, and no money or personal data. The
worst outcome of a breach is a defaced map or a burned demo instance. Re-check
this tier if Route ever ingests real courier positions or real customer addresses;
that moves it to T3 immediately (location data is personal data).

**Scaling: none.** Route is a portfolio demo with a fixed, small audience. The load
testing in Section 11 is a feature being demonstrated, not production scale-out,
and needs none of the scaling checklist. Re-check if the demo is ever put in front
of real traffic.

### Secrets and keys

- [ ] Every secret (database password, Redpanda credentials, the demo bearer
  token, any cloud credentials) stays server-side. The dashboard bundles nothing
  secret; it only calls `routed`.
- [ ] `.env` and secret files are in `.gitignore` before the first `git add`. If
  a secret is ever committed, rotate it; do not just delete the file.

### Database and data access

- [ ] All Postgres access goes through `pgx` parameterized queries. No SQL built
  by string concatenation, including the PostGIS spatial queries.
- [ ] Postgres and Redis are not reachable from the open internet. Both bind only
  to the Docker network; only `routed` is exposed on the instance.

### Auth and access control

- [ ] Mutating endpoints (`POST /orders`, `/orders/{id}/cancel`, `/sim/*`) require
  the bearer token, checked server-side. Read endpoints and the WebSocket are public.
- [ ] Courier gRPC sessions present a per-courier token. A session cannot send
  pings or responses for a different `courier_id` than the one its token names.
- [ ] `POST /orders` accepts only the documented fields. Stray fields such as
  `status`, `priority` above the allowed range, or `assigned_courier` are rejected.

### Rate limiting and abuse

- [ ] Rate-limit `POST /orders` per token and per IP, server-side, so the public
  demo cannot be turned into a load generator against itself.
- [ ] Cap concurrent WebSocket connections and the per-connection delta rate.
- [ ] Cap the number of concurrent courier gRPC sessions at the configured
  simulator maximum plus a small margin.

### Input and output handling

- [ ] Validate every `POST /orders` field server-side: coordinates inside the
  configured bounding box, priority in range, idempotency key present and bounded
  in length.
- [ ] Validate every ping: coordinates in range, `seq` monotonic, `client_ts`
  within a sanity window. Reject and count anything outside.
- [ ] Never write raw client-supplied strings (courier display names, idempotency
  keys) into log lines unescaped.
- [ ] Dashboard renders courier names through React's default escaping. No
  `dangerouslySetInnerHTML`.

### Dependencies

- [ ] Run `govulncheck` and `npm audit` before the first public deploy and on the
  CI schedule. Patch what has a non-breaking fix.
- [ ] Confirm `uber/h3-go`, the MapLibre packages, and every other dependency
  are the real registry packages before installing.

### Deployment and operations

- [ ] HTTPS only on the public demo; HTTP redirects.
- [ ] Standard headers on the dashboard: `Content-Security-Policy`,
  `X-Frame-Options`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`,
  `Strict-Transport-Security`.
- [ ] Debug endpoints, pprof, and source maps are off in the public deployment.
  `/metrics` is either internal-only or behind the bearer token.
- [ ] Clients receive generic error messages. Stack traces and internal errors go
  to logs only.
- [ ] `.git` is not served and is not present in the deployed image.
