# ADR 005: Pings are droppable, orders are not

## Decision
Under pressure, Route sheds courier pings and never sheds orders.

- Each courier stream has a buffer of one ping. If a new ping arrives while the
  previous one is still waiting, the new one replaces it, and
  `route_pings_shed_total` goes up. A position is last-write-wins state, so the
  skipped ping loses nothing.
- Pings that fail validation are dropped and counted by reason in
  `route_pings_rejected_total`. Pings with a lower `seq` than one already seen
  are dropped and counted in `route_out_of_order_pings_total`.
- Position history goes through a bounded channel. When it is full, rows are
  dropped rather than slowing the ping path.
- An order is written to Postgres before the API answers 201, inside one
  transaction with its outbox event. After that it cannot be lost.
- The in-memory wake-up queue for the matcher has a fixed size. If it is full, a
  wake-up is dropped, not an order. The order is still in Postgres as `queued`,
  and the sweep (every 2 seconds) triggers any order that has waited more than 5
  seconds.

## Context
Pings arrive at thousands per second and each makes the previous one obsolete.
Orders arrive more slowly and each one is a promise to a customer.

## Alternatives considered
- **Unbounded buffers.** Nothing is dropped, and memory grows until the process
  falls over. Latency grows without limit first.
- **Drop orders under load.** Easy, and it turns a slow system into one that
  quietly loses work.

## Consequences
- The ping-flood scenario reports how many pings were shed. Shedding is
  acceptable there. Match latency is the thing that must not suffer.
- The count of shed pings is the signal that the index is falling behind. The
  index lag histogram (`route_ping_index_lag_seconds`) says by how much.
- A dropped wake-up delays an order by up to about 5 seconds. It does not lose it.
