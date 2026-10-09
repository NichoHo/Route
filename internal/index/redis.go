package index

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"route/internal/config"
	"route/internal/metrics"

	"github.com/redis/go-redis/v9"
)

type Store struct {
	client       *redis.Client
	stalenessTTL time.Duration
}

func NewStore(addr string, stalenessTTL time.Duration) *Store {
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	return &Store{
		client:       rdb,
		stalenessTTL: stalenessTTL,
	}
}

func (s *Store) Close() error {
	return s.client.Close()
}

func (s *Store) GetClient() *redis.Client {
	return s.client
}

type CourierState struct {
	ID      string  `json:"id"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Heading float64 `json:"heading"`
}

func (s *Store) UpdatePosition(ctx context.Context, courierID string, lat, lng, heading float64) error {
	pipe := s.client.Pipeline()

	// Add to GEO index
	pipe.GeoAdd(ctx, "route:couriers", &redis.GeoLocation{
		Name:      courierID,
		Longitude: lng,
		Latitude:  lat,
	})

	stateBytes, _ := json.Marshal(CourierState{
		ID:      courierID,
		Lat:     lat,
		Lng:     lng,
		Heading: heading,
	})
	key := fmt.Sprintf("route:courier:%s:seen", courierID)
	pipe.Set(ctx, key, string(stateBytes), s.stalenessTTL)

	_, err := pipe.Exec(ctx)
	return err
}

// Point is a place to search for couriers around.
type Point struct{ Lat, Lng float64 }

// FindCandidates returns up to limit free couriers near a point, nearest
// first. It widens the search radius until it has at least minCandidates, up
// to config.MaxCandidateRadiusKm.
func (s *Store) FindCandidates(ctx context.Context, lat, lng float64, limit int, minCandidates int) ([]CourierState, error) {
	res, err := s.FindCandidatesBatch(ctx, []Point{{lat, lng}}, limit, minCandidates)
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// searchGridDeg is the grid orders are grouped on: about 330m. Orders in the
// same grid square share one GEOSEARCH, centred on the group and widened by
// the group's own spread, so a hotspot of a hundred orders costs a few
// searches instead of a hundred. A search was 89% of the Redis server's CPU in
// the throughput load test, at 325us a call.
const searchGridDeg = 0.003

// candidateRadiiKm is the ladder of search radii. It starts small: Redis sorts
// every courier inside the radius, so the first step decides the cost, and a
// dense fleet usually has enough free couriers within half a kilometre.
func candidateRadiiKm() []float64 {
	return []float64{0.5, 1, 2, 4, config.MaxCandidateRadiusKm}
}

type searchGroup struct {
	center  Point
	slackKm float64 // the farthest member from the center
	members []int   // indexes into the caller's points
}

// FindCandidatesBatch is FindCandidates for many points at once. Points close
// together are searched as one group (see searchGridDeg); the searches for all
// groups share round trips (one pipeline per radius step), and each courier's
// state and claim are read once however many groups see them. A group of n
// points gets limit+n candidates, so a hotspot's orders are not all fighting
// over the same few couriers. result[i] is for points[i].
func (s *Store) FindCandidatesBatch(ctx context.Context, points []Point, limit int, minCandidates int) ([][]CourierState, error) {
	results := make([][]CourierState, len(points))

	// Group the points by grid square.
	byCell := make(map[[2]int]*searchGroup)
	var groups []*searchGroup
	for i, p := range points {
		key := [2]int{int(math.Floor(p.Lat / searchGridDeg)), int(math.Floor(p.Lng / searchGridDeg))}
		g, ok := byCell[key]
		if !ok {
			g = &searchGroup{}
			byCell[key] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, i)
	}
	for _, g := range groups {
		var lat, lng float64
		for _, i := range g.members {
			lat += points[i].Lat
			lng += points[i].Lng
		}
		g.center = Point{lat / float64(len(g.members)), lng / float64(len(g.members))}
		for _, i := range g.members {
			if d := approxKm(g.center, points[i]); d > g.slackKm {
				g.slackKm = d
			}
		}
	}

	pending := make([]int, len(groups))
	for i := range pending {
		pending[i] = i
	}
	// What has been read so far, shared by every group and every radius.
	states := make(map[string]*CourierState) // nil means stale: no seen key
	claimed := make(map[string]bool)
	stale := make(map[string]struct{})

	for step, radius := range candidateRadiiKm() {
		if len(pending) == 0 {
			break
		}
		metrics.CandidateSearchesTotal.WithLabelValues(strconv.Itoa(step)).Add(float64(len(pending)))
		pipe := s.client.Pipeline()
		cmds := make([]*redis.GeoSearchLocationCmd, len(pending))
		for k, gi := range pending {
			g := groups[gi]
			want := limit + len(g.members)
			cmds[k] = pipe.GeoSearchLocation(ctx, "route:couriers", &redis.GeoSearchLocationQuery{
				GeoSearchQuery: redis.GeoSearchQuery{
					Longitude: g.center.Lng,
					Latitude:  g.center.Lat,
					// Widened by the group's spread so every member's own
					// radius is covered.
					Radius:     radius + g.slackKm,
					RadiusUnit: "km",
					Sort:       "ASC",
					// The cap doubles with each step. Capped at a fixed size, a
					// wider radius returned the same nearest couriers again, and
					// near a hotspot those are mostly busy: every search walked
					// the whole ladder to 8km and found nothing new.
					Count: (want * 3) << step,
				},
				// WithDist isn't used for its value here, but go-redis's
				// GeoSearchLocationCmd.readReply unconditionally parses each
				// result as a nested array, which Redis only produces when at
				// least one WITH* option is requested. Without this, GEOSEARCH
				// returns a flat array of names and the client fails to decode
				// the reply.
				WithDist: true,
			})
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return nil, err
		}

		// Read the state and claim of every courier not seen yet, in one go.
		var unread []string
		for _, cmd := range cmds {
			for _, loc := range cmd.Val() {
				if _, known := states[loc.Name]; !known {
					states[loc.Name] = nil
					unread = append(unread, loc.Name)
				}
			}
		}
		if err := s.readStates(ctx, unread, states, claimed, stale); err != nil {
			return nil, err
		}

		var stillPending []int
		for k, gi := range pending {
			g := groups[gi]
			want := limit + len(g.members)
			var candidates []CourierState
			for _, loc := range cmds[k].Val() {
				st := states[loc.Name]
				if st == nil || claimed[loc.Name] {
					continue // stale, or already claimed
				}
				candidates = append(candidates, *st)
				if len(candidates) >= want {
					break
				}
			}
			for _, i := range g.members {
				results[i] = candidates
			}
			if len(candidates) < want && len(candidates) < minCandidates {
				stillPending = append(stillPending, gi)
			}
		}
		pending = stillPending
	}

	if len(stale) > 0 {
		names := make([]interface{}, 0, len(stale))
		for n := range stale {
			names = append(names, n)
		}
		s.client.ZRem(ctx, "route:couriers", names...)
	}
	return results, nil
}

// approxKm is the distance between two nearby points in kilometers, using the
// flat-earth approximation. It is only used to size a search's slack.
func approxKm(a, b Point) float64 {
	dy := (b.Lat - a.Lat) * 111.32
	dx := (b.Lng - a.Lng) * 111.32 * math.Cos(a.Lat*math.Pi/180)
	return math.Hypot(dx, dy)
}

// readStates fills in the state and claim of each named courier, using MGET
// in chunks so no single command is huge.
func (s *Store) readStates(ctx context.Context, names []string, states map[string]*CourierState, claimed map[string]bool, stale map[string]struct{}) error {
	const chunk = 500
	for start := 0; start < len(names); start += chunk {
		end := start + chunk
		if end > len(names) {
			end = len(names)
		}
		part := names[start:end]
		seenKeys := make([]string, len(part))
		claimKeys := make([]string, len(part))
		for i, n := range part {
			seenKeys[i] = fmt.Sprintf("route:courier:%s:seen", n)
			claimKeys[i] = fmt.Sprintf("route:claim:%s", n)
		}
		pipe := s.client.Pipeline()
		seenCmd := pipe.MGet(ctx, seenKeys...)
		claimCmd := pipe.MGet(ctx, claimKeys...)
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return err
		}
		for i, n := range part {
			v := seenCmd.Val()[i]
			if v == nil {
				stale[n] = struct{}{}
				continue
			}
			var st CourierState
			if err := json.Unmarshal([]byte(v.(string)), &st); err != nil {
				continue
			}
			states[n] = &st
			if claimCmd.Val()[i] != nil {
				claimed[n] = true
			}
		}
	}
	return nil
}

func (s *Store) GetAllActiveCouriers(ctx context.Context) ([]CourierState, error) {
	// For demo purpose, fetch everything inside 1000km of Jakarta
	res, err := s.client.GeoSearchLocation(ctx, "route:couriers", &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Longitude:  config.JakartaCenterLng,
			Latitude:   config.JakartaCenterLat,
			Radius:     1000,
			RadiusUnit: "km",
			Sort:       "ASC",
			Count:      5000,
		},
		WithDist: true, // see the comment in FindCandidates
	}).Result()
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}

	keys := make([]string, len(res))
	for i, loc := range res {
		keys[i] = fmt.Sprintf("route:courier:%s:seen", loc.Name)
	}

	vals, err := s.client.MGet(ctx, keys...).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}

	var states []CourierState
	var staleCouriers []interface{}
	for i, v := range vals {
		if v == nil {
			staleCouriers = append(staleCouriers, res[i].Name)
			continue
		}
		str := v.(string)
		var state CourierState
		if err := json.Unmarshal([]byte(str), &state); err == nil {
			states = append(states, state)
		}
	}

	if len(staleCouriers) > 0 {
		s.client.ZRem(ctx, "route:couriers", staleCouriers...)
	}

	return states, nil
}
