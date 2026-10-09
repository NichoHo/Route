# ADR 003: Courier claims in Redis, with Postgres as the backstop

## Decision
A courier is claimed before an offer is created, with `SET route:claim:{id}
{offer_id} NX PX {ttl}`. A claimed courier is never a candidate. The claim
moves from the offer id to the assignment id when the courier accepts, and is
released on decline, expiry, cancellation and completion. The TTL covers
crashes.

Postgres holds the same facts durably and enforces them without Redis:

- `assignments(order_id)` is unique: one assignment per order.
- A partial unique index on `assignments(courier_id) WHERE completed_at IS NULL`:
  one active assignment per courier.
- A partial unique index on `offers(courier_id) WHERE state = 'pending'`: one
  pending offer per courier.
- `CreateOffer` refuses a courier that already has an active assignment.
- Every order status change names the status it expects, for example
  `UPDATE orders SET status = 'offered' ... WHERE status = 'batched'`. A cancel
  that arrives while a batch is solving cannot be overwritten.

Redis is the authority for who is eligible on the hot path. Postgres is the
record. A reconciliation job compares them at startup and every 30 seconds. It
restores a missing claim for every pending offer and active assignment, and it
counts any claim that holds some other value (`route_claims_reconciled_total`).

## Context
Two batches can solve at the same moment and both pick the same courier. The
claim decides which one wins. It must be atomic, fast, and safe when more than
one `routed` instance runs.

## Alternatives considered
- **`SELECT ... FOR UPDATE` in Postgres only.** Safe, but a lock held through
  candidate gathering and solving makes every batch wait for every other batch.
- **An in-process lock.** Fine for one instance. It breaks the first time there
  are two.

## What the chaos tests found
The first version trusted Redis too much. The tests in
`internal/matcher/chaos_test.go` cut the Redis connection at every step of a
batch and found three real bugs. All three are fixed, and the tests stay as
regression tests.

1. On accept, the claim was changed before the database commit. If Redis ran the
   change but the reply was lost, the transaction rolled back and the courier
   stayed claimed, for an hour, under an assignment that did not exist. The
   commit now comes first and the claim is fixed afterwards.
2. After Redis came back empty, a courier could hold a pending offer and an
   assignment at once. Postgres stopped a second assignment but not the extra
   offer. The offer index and the check in `CreateOffer` close this.
3. After Redis lost its data, the matcher kept offering busy couriers. Their
   accepts failed on the unique index, and the orders ran out of attempts
   without being assigned. A refused `CreateOffer` now repairs that courier's
   claim from Postgres and re-queues the order without using an attempt.

## What the load test found
Reconciliation itself had a race. It reads the live offers and assignments, then
restores their claims one by one. At 50 orders a second, some of them finish in
between, and their release has already run. The restore then created a claim for
something that no longer existed. For an assignment that claim lasts an hour, so
the courier sat out. The baseline run showed it as 34 restored claims in a run
with no faults. After every restore, reconciliation now checks Postgres again and
undoes the restore if the offer or assignment is gone. `TestReconcileDoesNot...`
in `internal/offers/lifecycle_test.go` finishes an assignment in exactly that gap.

## Consequences
- Until reconciliation next runs after Redis loses its data (up to 30 s),
  Postgres still prevents double booking, but the matcher can waste some batch
  attempts on couriers it should skip. No order is lost.
- Reconciliation can raise a false mismatch for a moment while an accept is in
  progress. It only counts and logs. It never overwrites a claim.
- The pending-offer index was added after the first database existed. An old
  database needs it applied by hand. The command is in `deploy/postgres/init.sql`.
