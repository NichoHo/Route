# ADR 010: Scoring weights

## Decision
The matcher scores each (order, courier) pair with a cost. Lower is better. The
cost is the sum of five terms. Every term is zero or more, and every weight is
in meters of pickup distance, so the weights can be compared with each other.

| Term | Formula | Default weight | Meaning of the default |
|---|---|---|---|
| Distance | `w_dist * meters` | 1 per meter | The unit. |
| Wait | `w_wait * seconds left under the cap` | 10 per second, cap 60 s | An order that has waited 40 s counts as 400 m closer than a new one. |
| Accept | `w_accept * (1 - accept rate)` | 500 | A courier who never accepts counts as 500 m farther away than one who always does. |
| Load | `w_load * (1 - zone pressure)` | 300 | An order in a zone with the highest demand pressure counts as up to 300 m closer. |
| Priority | `w_priority * (9 - priority)` | 100 per level | Each retry raises priority by one level, worth 100 m. |

Set the weights with the `SCORE_W_DIST`, `SCORE_W_WAIT`, `SCORE_WAIT_CAP`,
`SCORE_W_ACCEPT`, `SCORE_W_LOAD` and `SCORE_W_PRIORITY` environment variables.
A weight of 0 turns that term off. The function is `matcher.Cost`. It is pure
and has no database or clock access.

## Context
ROUTE_SPEC.md section 6.4 gives a four-term formula and says the defaults need
an ADR. The first version of the code scored only distance, scaled by a surge
ratio. It ignored order age, courier reliability and order priority.

## What the terms can and cannot do
Four of the five terms do not depend on which courier is chosen. The wait,
load and priority terms belong to the order. They add the same amount to every
courier for that order. So they cannot change which courier an order gets.

They matter when there are fewer couriers than orders. Then the solver must
choose which orders to serve, and these terms decide it: old, urgent orders in
busy zones go first. When couriers are plentiful, only distance and the accept
rate change the result.

## Differences from the spec
- **Wait term.** The spec uses a negative weight on age. This ADR uses a
  positive weight on the time left under a cap. Orders rank the same way. Costs
  stay at zero or above, which the Hungarian solver expects and which keeps
  batch totals easy to compare. After the cap, extra waiting earns nothing more.
- **Load term.** The spec uses the supply and demand ratio at the pickup. The
  code uses the zone's surge multiplier, mapped onto 0 to 1. The multiplier is
  already a step function of that ratio.
- **Priority term.** The spec does not list it in the formula, but it says that
  declined, expired and conflicted orders get a boosted priority. The term makes
  that boost have an effect.
- **Removed.** The first version multiplied distance by the ratio of the
  courier's zone surge to the order's zone surge. It is not in the spec and it
  is gone.

## Accept rate
A courier's accept rate comes from their offers in the last hour: accepted
offers divided by accepted, declined and expired offers. Offers on orders that
were later cancelled are not counted, because the courier did not cause them.

The rate is smoothed toward 0.85 as if the courier had 5 extra offers at that
rate. A courier with no history scores as average. One decline does not ruin a
courier with a long good record.

## Alternatives considered
- **Multiply distance by penalty factors.** Easy to write, but the effect of a
  factor depends on the distance, so the weights cannot be read as plain trade
  offs. Adding terms in one unit is easier to tune and explain.
- **Learn the weights from outcomes.** This needs real data. Route has none, so
  it would tune the weights to the simulator.
- **Estimate pickup time instead of distance.** This needs a routing engine.
  See ADR 004 in the spec.

## Consequences
- The default weights are guesses, not measured values. There is no road network
  or real courier data to calibrate against. Treat them as a starting point.
- To tune them, run the solver comparison scenario from ROUTE_SPEC.md section 11
  and watch match latency, the exhausted-order rate and
  `route_batched_vs_greedy_ratio`.
- `route_batched_vs_greedy_ratio` now compares pickup distance, not cost. It can
  be above 1 when the wait, accept or priority terms outweigh distance.
- The batch row in Postgres still stores total cost. The `batch.solved` event
  carries both distance and cost.
- Accept rates need an index on `offers (courier_id, resolved_at)`. New
  databases get it from `init.sql`. Existing databases can apply the command in
  that file.
