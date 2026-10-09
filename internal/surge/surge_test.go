package surge

import (
	"context"
	"testing"

	"github.com/uber/h3-go/v4"
)

type mockPublisher struct{}

func (m *mockPublisher) Publish(ctx context.Context, topic string, payload []byte) error {
	return nil
}

func TestZonesOnlyHotCellsWithClosedHexagonOutlines(t *testing.T) {
	e := NewEngine(nil, nil, nil)
	defer e.Stop()
	hot, _ := h3.LatLngToCell(h3.NewLatLng(-6.2, 106.82), 8)
	calm, _ := h3.LatLngToCell(h3.NewLatLng(-6.25, 106.9), 8)
	e.demandMu.Lock()
	e.multiplier[hot] = 2.0
	e.multiplier[calm] = 1.0
	e.demandMu.Unlock()

	zones := e.Zones()
	if len(zones) != 1 || zones[0].ID != hot.String() || zones[0].Multiplier != 2.0 {
		t.Fatalf("zones = %+v, want only the hot cell", zones)
	}
	ring := zones[0].Ring
	if len(ring) != 7 || ring[0] != ring[6] {
		t.Fatalf("outline has %d points, first=%v last=%v; want a closed hexagon (7 points)", len(ring), ring[0], ring[len(ring)-1])
	}
	for _, p := range ring {
		if p[0] < 106.80 || p[0] > 106.84 || p[1] < -6.22 || p[1] > -6.18 { // [lng, lat]
			t.Errorf("outline point %v is nowhere near the cell", p)
		}
	}
	if again := e.Zones(); len(again) != 1 || &again[0].Ring[0] != &ring[0] {
		t.Error("outline should be cached, not recomputed")
	}
}

func TestSurgeInvariants(t *testing.T) {

	// Test multiplier step function bounds
	cases := []struct {
		d        int
		s        int
		expected float64
	}{
		{0, 10, 1.0},
		{0, 0, 1.0},
		{1, 5, 1.0},  // ratio 0.2
		{1, 2, 1.2},  // ratio 0.5
		{5, 5, 1.5},  // ratio 1.0
		{10, 5, 2.0}, // ratio 2.0
		{20, 5, 3.0}, // ratio 4.0
		{10, 0, 3.0}, // ratio 10/1
	}

	for _, c := range cases {
		mult := CalculateMultiplier(c.d, c.s)

		if mult != c.expected {
			t.Errorf("Expected %f for d=%d, s=%d, got %f", c.expected, c.d, c.s, mult)
		}
		if mult < 1.0 || mult > 3.0 {
			t.Errorf("Multiplier out of bounds: %f", mult)
		}
	}
}
