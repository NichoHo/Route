# ADR 006: One `routed` process, with claims that are safe across processes

## Decision
Route runs as one `routed` process: ingest, index, matcher, offers, surge and the
HTTP, gRPC and WebSocket servers together. The one part built to survive a split
is the claim protocol (ADR 003). It is tested with 50 concurrent solvers against
100 couriers, and with Redis cut off mid-batch.

## Context
Splitting the matcher into its own service is easy to propose and expensive to
do. It should wait for evidence that one process is not enough.

## Alternatives considered
- **Split the matcher on day one.** More moving parts to run and debug before
  anyone knows whether they are needed.
- **Split ingest on day one.** Same.

## What is and is not safe to run twice
Safe, because the database or Redis settles any conflict:

- claiming a courier, and the Postgres indexes behind it
- accepting an offer (one conditional `UPDATE` wins)
- the sweep, which uses `UPDATE ... RETURNING` and is idempotent
- the outbox relay, which uses `FOR UPDATE SKIP LOCKED`

Not safe yet, because the state lives in one process:

- **The stream registry.** An offer is pushed down the courier's gRPC stream,
  which exists on one instance only. A second instance's matcher could pick that
  courier and have no way to reach it. Offers would need to travel over Redis
  pub/sub to the instance holding the stream.
- **The surge demand window.** Counted in memory per instance. It would move to
  Redis.
- **The batcher queue.** Per instance. Two instances would each batch only the
  orders their own API received, unless the queue is shared.

## Consequences
- The load results show where one instance runs out. They are in
  `loadtest/results/`. A missed target is written down with its cause.
- A single `routed` was limited by how the matcher did its work, not by the
  hardware. The first throughput run (500 orders a second) handled about 125
  orders a second, because each batch looked up candidates and created offers
  one order at a time. Running those steps concurrently inside a batch (16 at a
  time) fixed the first-order limit. Where one instance runs out now is in
  `loadtest/results/README.md`.
- The outbox relay used to move at most 1,000 events a second (100 per 100 ms
  tick). A busy run makes several events per order, so it now drains in chunks of
  1,000 until the backlog is gone. It is still one relay per instance, and it is
  asynchronous: it does not slow matching.
- Splitting the matcher out is a later change, driven by those numbers.
