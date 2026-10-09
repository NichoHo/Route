# ADR 001: Redis GEO for the live index, H3 for zoning, PostGIS for history only

## Decision
- The live position of every courier is in one Redis GEO set, `route:couriers`.
  Candidate search is `GEOSEARCH` by radius from the order's pickup, nearest
  first.
- A second key per courier, `route:courier:{id}:seen`, holds the courier's last
  state and expires after the staleness timeout (15 s). A courier whose `seen`
  key has expired is not a candidate, and the search removes it from the GEO set.
- The search tries radii of 0.5, 1, 2, 4 and 8 km in turn. It stops at the first
  radius that gives enough free couriers. 8 km is also the hard cap: the solver
  treats any courier farther than that as unreachable. The cap on how many
  results Redis returns doubles with each step, because a wider radius with the
  same cap just returns the same nearest couriers again (see below).
- Orders in the same 330 m grid square share one search, widened by the group's
  spread. A batch of orders in a hotspot costs a few searches, not one each.
- H3 (resolution 8, cells of about 0.7 km2) gives the zones for surge pricing
  and for the dashboard heatmap.
- PostGIS stores order locations and the position history. History is written
  in the background and is never on the matching path.

## Context
The matcher needs "the nearest available couriers to this point" many times a
second, over thousands of moving points, while every courier updates its
position every few seconds.

## Alternatives considered
- **H3 only, in memory.** Fast, but a restart loses every position, and the
  index is private to one process.
- **PostGIS on the hot path.** Correct and durable, but every ping becomes a
  database write and every match a spatial query against a table that changes
  constantly.
- **A custom quadtree.** No external dependency, but it is code to write, test
  and keep correct, and Redis GEO already does the job.

## Consequences
- Redis can lose its data. The index then rebuilds itself within one ping
  interval, because every ping rewrites the courier's position. Claims are a
  different matter. See ADR 003.
- `GEOADD` rejects latitudes beyond 85.05112878. Ping validation rejects them
  first, so one bad ping cannot fail a pipeline.
- Radius search measures distance in a straight line. See ADR 004.
- The history table is declared `PARTITION BY RANGE (ts)` but has only a
  default partition. Nothing creates dated partitions yet, so history grows in
  one piece. History writes are best effort: a full buffer drops rows rather
  than slowing pings.
- Searching is the matcher's main cost on Redis. In the throughput load test
  `GEOSEARCH` was 80 to 90 percent of Redis's CPU, at about 300 microseconds a
  call, because Redis sorts every courier inside the radius. Two mistakes made it
  worse and are fixed: a fixed result cap (a wider radius found nothing new near
  a hotspot, where the nearest couriers are mostly busy, so every search walked
  the ladder to 8 km), and one search per order.
- The remaining cost is real hotspot depletion: busy couriers stay in the GEO
  set until they are filtered out after the search. The next fix is a second set
  that holds only free couriers, updated when a claim is taken or released, so a
  search never sees a busy one. It is not built.
- What would change this decision: a courier population large enough that one
  Redis node cannot hold or search the set. Then shard the set by H3 cell.
