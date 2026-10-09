# ADR 007: Server time decides staleness and deadlines

## Decision
Only the server's clock is used to decide whether something is late.

- A courier is stale when its Redis `seen` key expires. Redis counts the TTL.
- An offer's deadline is set by the server when the offer is created, and the
  sweep compares it with the server's `NOW()`.
- Position history is stamped with the time the server received the ping, not
  the time on the courier's phone.
- A ping carries `client_ts`, and the server checks it only for sanity: it must
  be within five minutes of server time. That bound is far wider than normal
  clock drift on purpose. It rejects a ping with a timestamp of 0 or a date in
  another year. It does not judge freshness.

## Context
Phones have wrong clocks. A courier whose clock is 30 seconds fast would look
fresh forever if the server trusted it, and one whose clock is 30 seconds slow
would look stale and never be offered work.

## Alternatives considered
- **Trust client timestamps.** Simple, and wrong for any courier with a bad clock.
- **Hybrid logical clocks.** Correct for ordering events across machines. More
  than a system with one server clock needs.

## Consequences
- `TestChaosClockSkew` runs couriers with clocks 30 seconds fast and 30 seconds
  slow. Both are indexed, both are candidates and both get orders.
- Ordering of a courier's own pings uses `seq`, not time. A ping with a lower
  `seq` is dropped.
- Network delay counts against a courier: a ping that took two seconds to arrive
  refreshes the `seen` key two seconds late. That is the honest picture.
