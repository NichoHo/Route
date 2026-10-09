# ADR 009: Tracing and Load Generation

## Decision
Route uses OpenTelemetry with a direct OTLP exporter to Jaeger for distributed
tracing. `routesim` generates the demo's courier traffic and order stream over
gRPC and HTTP; `k6` (`loadtest/scenarios/k6_http.js`) is a separate script for
load-testing `POST /orders` directly.

## Context
The system needs a way to follow one order from `POST /orders` through to the
offer being sent, and a way to generate load for the scenarios in
ROUTE_SPEC.md section 11.

## Alternatives considered
- **Jaeger all-in-one vs. an OpenTelemetry Collector.** Jaeger all-in-one is a
  single container, which keeps Compose simpler for a demo environment.
- **k6 vs. routesim for load.** `routesim` already generates orders as part of
  running the demo, but `k6` is built specifically to drive HTTP load and
  report latency percentiles, which makes it a better fit for stressing
  `POST /orders` on its own.

## Consequences
- OpenTelemetry spans cover `POST /orders` (`internal/api/http.go`),
  `SolveBatch` (`internal/matcher/solver.go`), and offer creation
  (`internal/offers/lifecycle.go`).
- Trace context (`traceparent`) is written into the `order.created` outbox
  payload so an external consumer of that event can continue the trace.
- Jaeger runs at `make up` (the `routed` service depends on it), and its UI is
  at `http://localhost:16686`.
- `routesim` runs the named scenario profiles in `loadtest/scenarios/`
  (ADR 008). With no `SIM_PROFILE` it still runs the plain demo load, tuned by
  `SIM_COURIERS` and `SIM_ORDER_INTERVAL`.
- `routebench` reads each run back from Postgres and the metrics endpoint and
  writes the results in `loadtest/results/`.
- The request trace and the matching trace are still two separate traces. The
  batcher solves on its own context, so the `POST /orders` span and the
  `SolveBatch` span are not linked. The `traceparent` stored in the outbox
  payload is not used to join them. Linking them would mean carrying the span
  context from the request into the queue the batcher reads.
