# Route

Route is a real-time dispatch engine. It takes a stream of delivery orders
and a set of moving couriers. It assigns each order to a courier within
seconds. It never double-books a courier. See [ROUTE_SPEC.md](ROUTE_SPEC.md)
for the full design.

Route is a simulation. Couriers are synthetic and move on a random walk, not
on real roads. Distance is great-circle (haversine), not routed. No money
moves. See ROUTE_SPEC.md section 1 ("Honest framing") for what that trades off.

## Architecture

- **`routed` (`cmd/routed`)**: the server. It ingests courier pings over gRPC
  and keeps a live Redis GEO index. It batches incoming orders and solves
  each batch with the Hungarian algorithm, falling back to greedy above a
  size threshold. It runs the offer lifecycle, computes per-zone surge, and
  serves the HTTP API, the dashboard's WebSocket, and the dashboard itself.
- **`routesim` (`cmd/routesim`)**: the client. It simulates couriers driving
  around a bounding box (default: central Jakarta) and generates orders as a
  Poisson process with hotspots, following a scenario profile. It is the demo,
  the load generator and the only courier client (see
  [ADR 008](docs/decisions/008-simulator-first-class.md)).
- **`routebench` (`cmd/routebench`)**: reads a load-test run back from Postgres
  and the metrics endpoint, checks it against the targets in the spec, and
  writes JSON and charts.
- **Redis claim protocol**: a courier is claimed with `SET NX PX` before an
  offer is created. Two concurrent batches can never offer the same courier
  to two orders. Postgres's partial unique index on
  `assignments(courier_id) WHERE completed_at IS NULL` is the durable
  backstop for the same invariant.
- **Background sweep**: expires offers past their deadline and re-triggers
  orders stuck in `queued`/`batched`, entirely from Postgres state. No
  in-process timer needs to survive a restart for the system to keep moving.
- **Dashboard**: a small static page (`web/dashboard/`, no build step) served
  directly by `routed` at `http://localhost:8080/`. It updates over a
  WebSocket at 4 Hz and draws couriers, orders, offers and a surge heatmap
  (hexagons for every zone above 1.0x, with the peak multiplier in the header).

## Running the stack

```bash
make up   # postgres, redis, redpanda, jaeger, routed, prometheus, grafana
make sim  # starts routesim against it
```

Open the dashboard at [http://localhost:8080/](http://localhost:8080/). Open
Jaeger at [http://localhost:16686](http://localhost:16686), Prometheus at
[http://localhost:9091](http://localhost:9091), and Grafana at
[http://localhost:3001](http://localhost:3001). Grafana has no dashboards or
datasource set up; point it at the Prometheus container yourself if you want
one.

`make down` stops the stack. `make sim` runs the plain demo load. To run a
scenario instead, name it:

```bash
make sim PROFILE=demo   # a calm stretch, then a rush into one hotspot (loops)
```

The scenarios are the JSON files in [loadtest/scenarios](loadtest/scenarios).
Without a profile, `SIM_COURIERS` (default 50) and `SIM_ORDER_INTERVAL` (default
`2s`, Go duration syntax) set the load. Set `BATCH_WINDOW` on `routed` to change
the batch window.

## HTTP API

| Route | Notes |
|---|---|
| `POST /orders` | Bearer token. Requires an `Idempotency-Key` header. A repeated key returns the original order unchanged. |
| `GET /orders/{id}` | |
| `POST /orders/{id}/cancel` | Bearer token. Only works from a pre-assigned state (`queued`/`batched`/`offered`). Releases any pending offer's claim. |
| `GET /couriers?status=&limit=&offset=` | Live courier state from Redis, not a separate table. |
| `GET /surge` | Current multiplier per zone. |
| `GET /batches?since=&limit=` | Recent solved batches with solver stats. |
| `POST /couriers/{id}/token` | Bearer token. Issues the gRPC session token for one courier. |
| `GET /healthz` | |
| `WS /live` | Dashboard snapshot stream (4 Hz). Same-origin only, 100 connections max. |

## Security

This is a T1/personal-tier demo with no real data (ROUTE_SPEC.md section 15).

- **Mutating endpoints** (`POST /orders`, `/orders/{id}/cancel`,
  `/couriers/{id}/token`) need `Authorization: Bearer $ROUTE_API_TOKEN`.
  Reads, the dashboard and `/live` are public.
- **Courier gRPC sessions** need `x-courier-id` and `authorization: Bearer
  <token>` metadata. A courier token is an HMAC of the courier id, keyed from
  the API token. It only works for that id, and a session can only send pings
  and offer responses for its own courier. `routesim` fetches its tokens from
  `POST /couriers/{id}/token` at start.
- **Rate limits** on the mutating endpoints: per client IP and per token.
- **`/metrics`** is on port 9100, which compose does not publish. Prometheus
  reaches it over the Docker network.
- **Dashboard headers.** Every response carries a Content-Security-Policy that
  allows only the dashboard's own files, MapLibre from unpkg (pinned with a
  Subresource Integrity hash), the Inter font, CARTO map data and the page's own
  WebSocket. It also sends `X-Frame-Options: DENY`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: no-referrer` and a restrictive Permissions-Policy.
  `Strict-Transport-Security` is sent when the request arrived over HTTPS,
  directly or through a proxy that sets `X-Forwarded-Proto`. `routed` does not
  terminate TLS itself, so HTTPS and the redirect from HTTP belong to whatever
  sits in front of it. Inline script is not allowed; inline styles are, because
  MapLibre sets style attributes from script.
- Postgres, Redis, Redpanda, Jaeger, Prometheus and Grafana bind to
  `127.0.0.1` only.

`routed` and `routesim` refuse to start without `ROUTE_API_TOKEN`
(`routed` also requires at least 16 characters). Compose falls back to the
dev-only value `dev-only-token-change-me`. For anything beyond your own
machine, set your own in `deploy/compose/.env`, which is gitignored:

```
ROUTE_API_TOKEN=<long random string>
```

| Variable | Default | Meaning |
|---|---|---|
| `ROUTE_API_TOKEN` | none (required) | Bearer token for mutating endpoints. Courier tokens derive from it. |
| `ORDER_RATE_PER_IP` | `1000` | Sustained requests/s per client IP on mutating endpoints (burst is 2x). |
| `ORDER_RATE_PER_TOKEN` | `1000` | Same, per token. |
| `MAX_COURIER_SESSIONS` | `5500` | Concurrent gRPC courier sessions. |

The defaults sit above the section 11 load scenarios so load tests run
unthrottled. Lower them for a public deployment. Per-IP limiting uses the
connection's peer address, so behind a reverse proxy it becomes one shared
bucket. Not done yet: TLS, security headers on the dashboard, and revoking an
individual courier token (rotate `ROUTE_API_TOKEN` to revoke all).

For k6, pass the token: `ROUTE_API_TOKEN=... ./run_k6.ps1`.

## Building and testing

Tests need Postgres and Redis. `make test` runs the full `gofmt`/`go
vet`/build/test cycle in a container against throwaway instances of both (the
`test` Compose profile). It never touches the databases the demo above uses:

```bash
make test
```

The suite includes the chaos and concurrency tests (Redis cut off at every step
of a batch, 50 concurrent solvers, clock skew) and the end-to-end scenario from
the spec. The end-to-end test takes about 30 seconds; `go test -short` skips it.
`make test` starts from fresh databases each time: `init.sql` only runs on a new
container, so a leftover one from before a schema change would fail every test.

If you edit `courier.proto`, run this to regenerate
`courier.pb.go`/`courier_grpc.pb.go`. The committed files are canonical;
nothing regenerates them at build or test time.

```bash
make proto
```

## When Redis is lost

Redis holds the hot-path courier claims; Postgres is the durable record. If
Redis restarts empty, `routed` repairs it from Postgres: `ReconcileClaims`
re-creates one claim per pending offer and per active assignment, at startup
and every 30 seconds (`route_claims_reconciled_total`). Until it runs,
Postgres still refuses a second offer to a busy courier (a partial unique
index on pending offers, plus a check for an active assignment), so a courier
is never double booked. The cost is a few wasted batch attempts, not lost
orders. The index is created by `deploy/postgres/init.sql`; a database made
before it existed needs it applied by hand (the command is in that file).

## Matching weights

Each (order, courier) pair gets a cost from five terms: pickup distance, order
age, the courier's recent accept rate, demand in the pickup zone, and order
priority. Lower cost wins. Every weight is in meters of distance, and you can
set it with an environment variable. A weight of 0 turns that term off.

| Variable | Default | Meaning |
|---|---|---|
| `SCORE_W_DIST` | `1` | Cost per meter of courier-to-pickup distance. |
| `SCORE_W_WAIT` | `10` | Cost per second an order has left before the cap. Older orders cost less. |
| `SCORE_WAIT_CAP` | `60s` | Age after which waiting earns nothing more. |
| `SCORE_W_ACCEPT` | `500` | Cost for a courier who never accepts. |
| `SCORE_W_LOAD` | `300` | Cost for an order in a zone with no demand pressure. |
| `SCORE_W_PRIORITY` | `100` | Cost per priority level below 9. Failed offers raise priority by one. |

The defaults are guesses, not calibrated values. The reasoning is in
[docs/decisions/010-scoring-weights.md](docs/decisions/010-scoring-weights.md).

## Scenarios and load tests

A scenario is a JSON profile: courier count, ping rate, how couriers behave
(accept rate, response delay, flaky couriers), hotspots, and phases that set the
order rate over time. The ones in [loadtest/scenarios](loadtest/scenarios) match
the table in ROUTE_SPEC.md section 11: `baseline`, `throughput`, `ping-flood`,
`hotspot-spike`, `contention` and `solver-comparison`, plus `e2e` and `demo`.

```bash
loadtest/run.sh baseline                                       # one scenario on a fresh stack
loadtest/run.sh solver-comparison solver-comparison-window-0ms 0   # label and batch window in ms
```

Each run starts its own stack under the compose project `routeload`, removes it
afterwards, and writes `<label>.json` and `<label>.svg` to
[loadtest/results](loadtest/results). The results README has the numbers, the
hardware they came from, and every target that was missed.

The simulator compresses time (couriers drive at hundreds of meters a second) so
supply turns over at benchmark order rates. ADR 008 explains why and what it
costs. The k6 script in `loadtest/scenarios/k6_http.js` is still there for
hammering `POST /orders` alone: `ROUTE_API_TOKEN=... ./run_k6.ps1`.

## Observability

- **Jaeger**: spans cover `POST /orders` (`http.go`), `SolveBatch`
  (`matcher/solver.go`) and offer creation (`offers/lifecycle.go`). The
  batcher solves on a fresh context, so the `POST /orders` trace and the
  `SolveBatch`/offer trace are separate. The request's `traceparent` is stored
  in the `order.created` outbox payload but is not yet used to link them.
- **Prometheus** (`/metrics` on `:9100`, internal to the Docker network): match latency and rate, batch size and solve
  time by solver, the batched-vs-greedy cost ratio (set only when both solvers
  match the same number of orders), queue depth, claim
  conflicts, shed/out-of-order ping counts, and offer outcomes.
- No Grafana dashboards are provisioned. Point Grafana or any Prometheus
  reader at `/metrics` if you want one.
