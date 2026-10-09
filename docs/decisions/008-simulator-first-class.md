# ADR 008: The simulator is a first-class component

## Decision
`routesim` is a real program with its own tests, not a test helper. It is
three things at once:

- **The demo.** `make sim PROFILE=demo` loops a calm stretch and a rush into one
  hotspot, so surge shows up on the map.
- **The load generator.** The scenarios in section 11 of the spec are committed
  profile files in `loadtest/scenarios/`. Each one fixes the courier count, the
  ping rate, how couriers behave, and how orders arrive over time.
- **The only courier client.** It speaks the real gRPC protocol with real
  tokens, so it exercises the same code a phone would.

A profile sets: courier count and ramp-up; ping interval; per-courier accept
probability drawn around a mean; response delay; a share of flaky couriers that
go silent for a while; hotspots with Gaussian spread; and phases. Each phase has
a duration, an order rate, and the share of orders that come from hotspots
(optionally one named hotspot). Orders arrive as a Poisson process. A file with
an unknown field or a bad range is rejected at load. A test loads every
committed profile.

`routebench` reads a run back out of Postgres and the metrics endpoint and
checks it against the targets in the spec. See `loadtest/results/README.md`.

## Context
The spec says to treat the simulator as a first-class component. A load test is
only worth reading if the load is realistic, repeatable and written down.

## Alternatives considered
- **Replay recorded data.** There is none to record.
- **k6 only.** Good for hammering `POST /orders`. It cannot play the courier side
  of the protocol. `loadtest/scenarios/k6_http.js` stays for that one job.

## Honest limits
- **Time is compressed.** Couriers drive at 500 m/s in the load profiles. At real
  speed, 1,000 couriers could finish about 3 orders a second, so the spec's 50
  orders a second would only measure how fast the fleet runs out. With
  compression, supply turns over fast enough that the numbers describe the
  matching system. The demo profile uses 60 m/s. Both are written in the
  profile files.
- Couriers move in straight lines inside the bounding box. There are no roads
  (ADR 004).
- Response delays in the load profiles are 0.5 to 3 s, shorter than the spec's
  default of 1 to 8 s, for the same reason.
- One process produces all the courier traffic and all the orders, on the same
  machine as the server. The simulator's own view of `POST /orders` latency is in
  its summary. Orders it could not send are counted as `dropped_by_client`, so a
  slow generator cannot hide as a fast server.
