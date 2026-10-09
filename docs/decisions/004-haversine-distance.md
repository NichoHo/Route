# ADR 004: Haversine distance, not road distance

## Decision
Every distance in the matcher is the great-circle (haversine) distance between
two points. The simulator moves couriers in straight lines. Nothing in Route
knows about roads.

## Context
A real dispatcher cares about travel time on roads. Getting it needs a map of
the road network and a routing engine. That is a large piece of work that does
not change what this project sets out to show: batching, claims, backpressure
and honest measurement.

## Alternatives considered
- **A routing engine (OSRM or Valhalla) as a sidecar.** The right answer for a
  real product. It needs road data for the area, a container to run, and a
  network call (or a cache) on the matching path.
- **Straight-line Euclidean distance on latitude and longitude.** Cheaper than
  haversine, but wrong away from the equator, and the saving is not worth it.

## Consequences
- Pickup distances are lower bounds. Real roads are longer, and one-way streets
  and rivers make some nearby couriers far in practice. The README and the spec
  say this plainly.
- The simulator is not a test of routing quality. It is a test of the matching
  system.
- Upgrade path: replace `Haversine` inside `matcher.Cost` with an estimated
  travel time from a routing engine's table service, asked once per batch for
  the order and courier pairs and cached by H3 cell pair. Keep haversine as the
  cheap filter that picks the candidates and enforces the 8 km cap.
