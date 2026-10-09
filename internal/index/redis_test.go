package index

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestStore(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not available at %s", addr)
	}

	store := NewStore(addr, 5*time.Second)
	defer store.Close()

	ctx := context.Background()

	// Clear out any old data
	rdb.Del(ctx, "route:couriers", "route:courier:c1:seen", "route:courier:c2:seen", "route:courier:c3:seen", "route:claim:c3")

	// c1: within 1km, available. c2: stale. c3: within 1km, but claimed.
	if err := store.UpdatePosition(ctx, "c1", -6.200, 106.825, 90.0); err != nil {
		t.Fatalf("UpdatePosition c1 failed: %v", err)
	}
	if err := store.UpdatePosition(ctx, "c2", -6.201, 106.826, 180.0); err != nil {
		t.Fatalf("UpdatePosition c2 failed: %v", err)
	}
	if err := store.UpdatePosition(ctx, "c3", -6.2001, 106.8251, 0.0); err != nil {
		t.Fatalf("UpdatePosition c3 failed: %v", err)
	}

	// Make c2 stale by deleting its seen key manually
	rdb.Del(ctx, "route:courier:c2:seen")
	// Claim c3 so it's excluded despite being fresh and nearby.
	rdb.Set(ctx, "route:claim:c3", "some-offer-id", 0)

	candidates, err := store.FindCandidates(ctx, -6.200, 106.825, 5, 1)
	if err != nil {
		t.Fatalf("FindCandidates failed: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("Expected 1 candidate, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].ID != "c1" {
		t.Errorf("Expected candidate c1, got %s", candidates[0].ID)
	}
}

// ROUTE_SPEC.md 10.2: radius expansion terminates and respects the cap. A
// courier beyond the outermost radius is never a candidate, however few
// candidates the search found.
func TestFindCandidatesRespectsRadiusCap(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not available at %s", addr)
	}
	store := NewStore(addr, 5*time.Second)
	defer store.Close()
	ctx := context.Background()
	rdb.Del(ctx, "route:couriers", "route:courier:r-near:seen", "route:courier:r-far:seen")

	// 0.027 degrees of latitude is about 3km; 0.108 is about 12km, beyond the 8km cap.
	if err := store.UpdatePosition(ctx, "r-near", -6.200+0.027, 106.825, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePosition(ctx, "r-far", -6.200+0.108, 106.825, 0); err != nil {
		t.Fatal(err)
	}

	// Wanting far more candidates than exist forces every radius step.
	got, err := store.FindCandidates(ctx, -6.200, 106.825, 50, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "r-near" {
		t.Fatalf("got %+v, want only r-near (r-far is beyond the radius cap)", got)
	}
}

// A hotspot's worth of orders must share searches. The throughput load test
// found GEOSEARCH was 89% of the Redis server's CPU when every order searched
// alone.
func TestFindCandidatesBatchSharesSearchesBetweenNearbyPoints(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis is not available at %s", addr)
	}
	store := NewStore(addr, 5*time.Second)
	defer store.Close()
	rdb.Del(ctx, "route:couriers")

	// 30 couriers around the hotspot.
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("g%02d", i)
		rdb.Del(ctx, "route:courier:"+id+":seen", "route:claim:"+id)
		if err := store.UpdatePosition(ctx, id, -6.2+float64(i)*0.0001, 106.82, 0); err != nil {
			t.Fatal(err)
		}
	}

	// 40 orders within about 100m of each other, and one beyond reach.
	var points []Point
	for i := 0; i < 40; i++ {
		points = append(points, Point{-6.2 + float64(i%8)*0.0001, 106.82 + float64(i/8)*0.0001})
	}
	points = append(points, Point{-6.2, 106.95}) // about 14km east, beyond the 8km search cap

	rdb.ConfigResetStat(ctx)
	got, err := store.FindCandidatesBatch(ctx, points, 5, 5)
	if err != nil {
		t.Fatal(err)
	}

	info, _ := rdb.Info(ctx, "commandstats").Result()
	calls := 0
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "cmdstat_geosearch:calls=") {
			fmt.Sscanf(strings.TrimPrefix(line, "cmdstat_geosearch:calls="), "%d", &calls)
		}
	}
	// Two groups. The far one finds nobody at any radius and walks the whole
	// ladder, so allow up to one search per group per step.
	if calls == 0 || calls > 2*len(candidateRadiiKm()) {
		t.Errorf("%d GEOSEARCH calls for 41 points in 2 places, want a handful, not 41", calls)
	}

	for i := 0; i < 40; i++ {
		if len(got[i]) < 5 {
			t.Fatalf("order %d got %d candidates, want at least 5", i, len(got[i]))
		}
	}
	if len(got[40]) != 0 {
		t.Errorf("the far order got %d candidates, want none", len(got[40]))
	}

	// One point behaves like the plain single search.
	one, err := store.FindCandidates(ctx, -6.2, 106.82, 5, 5)
	if err != nil || len(one) < 5 {
		t.Fatalf("single point search: %v, %v", one, err)
	}
}

// Near a hotspot the nearest couriers are mostly busy. A search that asks Redis
// for a fixed number of the nearest members gets the same busy ones at every
// radius and never reaches the free couriers behind them; every search then
// walked the whole radius ladder. The result cap must grow with the radius.
func TestFindCandidatesReachesFreeCouriersBehindBusyOnes(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis is not available at %s", addr)
	}
	store := NewStore(addr, 5*time.Second)
	defer store.Close()
	rdb.Del(ctx, "route:couriers")

	// 100 couriers within about 100m of the order, all busy (claimed).
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("busy%03d", i)
		if err := store.UpdatePosition(ctx, id, -6.2+float64(i%10)*0.0001, 106.82+float64(i/10)*0.0001, 0); err != nil {
			t.Fatal(err)
		}
		rdb.Set(ctx, "route:claim:"+id, "an-offer", time.Minute)
	}
	// 10 free couriers about 2km away.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("free%02d", i)
		rdb.Del(ctx, "route:claim:"+id)
		if err := store.UpdatePosition(ctx, id, -6.2+0.018+float64(i)*0.0001, 106.82, 0); err != nil {
			t.Fatal(err)
		}
	}

	// limit 5 asks Redis for 15 members at the first step, all of them busy.
	got, err := store.FindCandidates(ctx, -6.2, 106.82, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 5 {
		t.Fatalf("found %d free couriers, want at least 5: the search never got past the busy ones", len(got))
	}
	for _, c := range got {
		if strings.HasPrefix(c.ID, "busy") {
			t.Errorf("busy courier %s returned as a candidate", c.ID)
		}
	}
}
