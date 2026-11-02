# ADR 002: Batched matching, 2 second window, Hungarian with a greedy fallback

## Decision
The matcher collects orders for a window, then solves them together.

- **Window.** 2 seconds (`BATCH_WINDOW`). A batch also closes early when it
  holds `MAX_BATCH_SIZE` orders. The default is 200.
- **Solver.** The Hungarian algorithm gives the lowest total cost. It runs when
  `min(orders, couriers)^2 * max(orders, couriers)` is at most 2e8
  (`config.HungarianMaxWork`). Above that, the batch uses greedy: take the
  cheapest remaining (order, courier) pair until none is left.
- **Comparison.** Every batch also runs greedy and records both pickup
  distances. The live ratio is the metric `route_batched_vs_greedy_ratio`.
- **Passes.** An order that loses out because no courier is left for it is
  solved again at once, up to three passes, against the next-nearest free
  couriers. Orders still unserved go back to the queue.
- **One batch at a time.** `MAX_CONCURRENT_BATCHES` is 1.

## Context
Matching each order to its nearest courier on arrival is fast. It also gives
the first order a courier that a later, closer order needed. A window lets
the solver trade one long pickup for two short ones. The window adds latency to
every order. The decision is how much latency to spend and what it buys.

## Alternatives considered
- **Greedy only.** Simplest, no window. It is the 0 ms row below.
- **Always Hungarian.** The cost grows with the cube of the batch. Large
  batches take longer than the window.
- **Min-cost flow.** Handles the same problem. It is more code than the
  benchmarks justify, because the solver is not the cost (see below). It is the
  next step if Hungarian becomes the cost.

## What the load tests found

### The window buys distance and costs latency
The baseline load (1,000 couriers, 50 orders per second), two minutes per window:

| Window | Mean pickup | Against 0 ms | Added median latency |
|---|---|---|---|
| 0 ms | 443 m | none | none |
| 1000 ms | 380 m | -14.3% | +0.56 s |
| 2000 ms | 354 m | -20.2% | +0.91 s |
| 4000 ms | 328 m | -26.0% | +0.87 s |

The gain flattens. Going from 1 to 2 seconds adds 5.9 points of distance.
Going from 2 to 4 seconds adds 5.8 points for twice the window. Two seconds is
the spec's default, and the data does not argue for moving it. The live ratio over
the full runs was 92 to 93 percent of the greedy distance.

### Where one batch spends its time
Measured on 657 batches of 100 orders (throughput scenario, 5,000 couriers):

| Phase | Mean per batch |
|---|---|
| Candidate search (Redis) | 105 ms |
| Offers (Postgres and Redis) | 52 ms |
| Claim orders (Postgres) | 17 ms |
| Solve | 16 ms |
| Cost matrix | 15 ms |
| Accept rates | 6 ms |

The solver is not the cost. The solve phase took 16 ms per batch. Candidate
search and offer creation took most of the time.

### Batch size: larger is better, up to a point
The throughput scenario offers 500 orders per second. With batches of 100 the
matcher assigned 102,000 to 125,000 of 150,000 orders. The queue filled, and
orders that waited 2 minutes were given up.

| Setting | Match rate | Exhausted orders |
|---|---|---|
| 100 per batch, three runs | 83.0%, 79.7%, 68.5% | 25,580 to 46,881 |
| 200 per batch | 95.0% | 7,544 |
| 200 per batch, 2 batches at once | 88.0% | 18,052 |

The first row is the spread for identical code. The machine also runs the load
generator, so one run is not precise. The direction is clear. Each batch costs a
fixed amount of Redis and Postgres work, so fewer, larger batches get more
orders through. The default is now 200.

### Two batches at once is worse
Two concurrent batches raised claim conflicts from 4 to 66,975. Both batches
pick from the same free couriers. The loser re-queues its order and wastes the
work. The default stays at one.

### A queue-duplicate fix did nothing
The sweep re-triggers orders that are still waiting for a batch. This put
duplicate ids in the queue, and about 16 percent of batch slots held an order
already queued. The batcher now keeps each waiting id once. The match rate did
not change within the noise, so duplicates were not the limit. The fix stays
because it is correct and cheap.

## What is still open
At 500 orders per second the matcher is the limit. Redis ran at 43 to 63
percent of one core, and the whole machine at about 1,000 of 1,600 percent, so
neither is full. One batch at a time is the limit.

The fix is to split the matcher by zone. Each zone gets its own batcher and its
own set of couriers. Two batches then cannot pick the same courier, and
concurrency stops costing conflicts. This is not built. The throughput results
list the missed targets and this as the cause.

## Consequences
- Every order waits up to one window before it is offered. At 50 orders per
  second the median match latency is about 1 s.
- The batch cap bounds the work per batch. A spike queues up behind it and
  drains at the batch rate (see the hotspot spike results).
- The comparison against greedy runs on every batch. It costs one extra greedy
  solve, which is small next to candidate search.
- `MAX_BATCH_SIZE` and `MAX_CONCURRENT_BATCHES` are environment variables. A
  deployment can change them without a rebuild.
