# Engineering Route: the claim protocol, the sweep, and two bugs that only showed up when the stack actually ran

Route assigns delivery orders to couriers: a batch window collects a few
seconds of orders, an assignment problem gets solved over them, and each
winning pair gets an offer. Two invariants make or break a system shaped like
that: no courier is ever assigned two orders at once, and no order is ever
silently lost. Here's how those hold, and two bugs that made it into a
running stack anyway.

## The claim protocol

The naive way to stop double-booking is a Postgres row lock held across the
whole match: gather candidates, solve, decide, all under `SELECT ... FOR
UPDATE`. That serializes every batch behind every other batch's candidate
gathering, which is the one thing a batching matcher can't afford.

Route uses a two-tier authority model instead:

1. **Redis is the hot-path authority.** Before an offer is created, the
   solver claims the courier with `SET NX PX`:
   ```go
   ok, err := claimsMgr.AcquireOfferClaim(ctx, c.ID, offerID, config.OfferClaimTTL)
   ```
   `NX` makes this atomic: if the key already exists, the claim fails and
   costs nothing but a round trip.
2. **Postgres is the durable backstop.** A partial unique index,
   `assignments(courier_id) WHERE completed_at IS NULL`, makes double-booking
   impossible at the database level even if the Redis-side logic ever gets it
   wrong.

If a claim fails, the solver drops that pairing and leaves the order
`queued`: no retry loop, no immediate re-fight for the same courier. The
background sweep (next section) picks it up on its own schedule.

Accepting an offer swaps the claim rather than releasing it:
`ConvertOfferToAssignment` does a compare-and-set in one Lua script, so
there's no window where the courier is claimed by nothing between "offer
accepted" and "assignment recorded."

## The sweep: no in-process timer has to survive a restart

The first version of this used `time.AfterFunc` to expire offers after their
deadline. That works until `routed` restarts with a pending offer in flight:
the timer is gone, and nothing ever expires that offer or frees the courier.

Route now runs a small periodic sweep against Postgres instead:

- Expire any `offers` row still `pending` past its `deadline_at`.
- Put any `orders` row stuck in `queued`/`batched` past a short staleness
  window back to `queued` and re-trigger it. (`SolveBatch` only picks up
  `queued` orders, so resetting the status is what makes the retry work.)
- Give up on (`exhausted`) any order that's been unmatched past a longer
  ceiling, so an order with no reachable courier doesn't get silently
  re-batched forever.

All three are ordinary `UPDATE ... RETURNING` statements. Restart `routed` at
any point and the sweep picks up exactly where the crashed process left off,
because the state it needs was never anywhere but Postgres.

## Two bugs that only showed up when the stack actually ran

Both of Route's dependencies have a default that's invisible from reading the
Go code, and both were only caught by bringing up the full Docker Compose
stack and reading logs, not by `go build`, `go vet`, or the test suite.

**go-redis's `GeoSearchLocation` requires a `WITH*` flag it doesn't use.**
Route's candidate query doesn't need the distance Redis can return alongside
each match, so it seemed safe to drop `WithDist: true` from the query. It
wasn't: `go-redis`'s reply parser unconditionally expects each result nested
as `[name, ...extras]`, a shape Redis only produces when at least one of
`WITHCOORD`/`WITHDIST`/`WITHHASH` is requested. Drop them all and Redis
returns a flat list of names instead, and the client fails with `can't parse
array/set/push reply`. The fix is one line back, with a comment explaining
why a field that's never read still has to be requested.

**franz-go doesn't ask Kafka-compatible brokers to auto-create topics.**
`kgo.NewClient` defaults `AllowAutoTopicCreation` to `false`, independent of
whatever the broker itself allows. Nothing in Route ever explicitly created
its eight Kafka topics, so every `ProduceSync` failed with
`UNKNOWN_TOPIC_OR_PARTITION`. No event had ever actually reached Redpanda.
The outbox table looked fine the whole time (writes there always succeeded);
the failure was purely on the relay's produce path, logged and otherwise
invisible. One client option, `kgo.AllowAutoTopicCreation()`, fixes it.

You cannot find either bug by reading the Go source next to the libraries'
type signatures. You find them by running the real stack and watching
`routed`'s logs while orders actually flow through it.

## What the running system looks like

With the default profile (50 couriers, one order every 2 seconds), a
representative sample after a few minutes: 169 orders matched, zero left
`queued` or `exhausted`, zero couriers holding more than one active
assignment, and match latency under 100ms for 83% of orders (average ~0.8s,
tail pulled up by the handful that needed a second offer round). None of that
is a load test. It is a demo profile confirming the invariants hold under
ordinary running conditions, not under contention. The load scenarios in
`ROUTE_SPEC.md` section 11 are what would actually stress the claim protocol
and the batch window.
