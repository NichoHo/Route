package surge

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber/h3-go/v4"

	"route/internal/index"
)

// Publisher interface for outbox/events
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// MaxMultiplier is the top step of CalculateMultiplier.
const MaxMultiplier = 3.0

func CalculateMultiplier(d, s int) float64 {
	denom := s
	if denom < 1 {
		denom = 1
	}
	ratio := float64(d) / float64(denom)

	if d == 0 {
		return 1.0
	}
	if ratio >= 3.0 {
		return MaxMultiplier
	} else if ratio >= 2.0 {
		return 2.0
	} else if ratio >= 1.0 {
		return 1.5
	} else if ratio >= 0.5 {
		return 1.2
	}
	return 1.0
}

type Engine struct {
	pool      *pgxpool.Pool
	store     *index.Store
	publisher Publisher

	// Sliding window for demand. h3 cell -> array of timestamps
	demandMu sync.Mutex
	demand   map[h3.Cell][]time.Time

	multiplier map[h3.Cell]float64

	// Hexagon outlines, computed once per cell, for the dashboard.
	ringMu sync.Mutex
	rings  map[h3.Cell][][2]float64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewEngine(pool *pgxpool.Pool, store *index.Store, publisher Publisher) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		pool:       pool,
		store:      store,
		publisher:  publisher,
		demand:     make(map[h3.Cell][]time.Time),
		multiplier: make(map[h3.Cell]float64),
		rings:      make(map[h3.Cell][][2]float64),
		ctx:        ctx,
		cancel:     cancel,
	}

	e.wg.Add(1)
	go e.loop()
	return e
}

func (e *Engine) RecordOrder(lat, lng float64) {
	cell, err := h3.LatLngToCell(h3.NewLatLng(lat, lng), 8)
	if err != nil {
		return
	}

	e.demandMu.Lock()
	defer e.demandMu.Unlock()
	e.demand[cell] = append(e.demand[cell], time.Now())
}

func (e *Engine) GetMultiplier(lat, lng float64) float64 {
	cell, err := h3.LatLngToCell(h3.NewLatLng(lat, lng), 8)
	if err != nil {
		return 1.0
	}

	e.demandMu.Lock()
	defer e.demandMu.Unlock()

	if mult, ok := e.multiplier[cell]; ok {
		return mult
	}
	return 1.0
}

func (e *Engine) loop() {
	defer e.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.compute()
		}
	}
}

func (e *Engine) compute() {
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)

	e.demandMu.Lock()
	// Cleanup old demand
	for cell, times := range e.demand {
		var valid []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(e.demand, cell)
		} else {
			e.demand[cell] = valid
		}
	}

	// Copy demand counts
	demandCounts := make(map[h3.Cell]int)
	for cell, times := range e.demand {
		demandCounts[cell] = len(times)
	}
	e.demandMu.Unlock()

	// Get Supply
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	couriers, err := e.store.GetAllActiveCouriers(ctx)
	if err != nil {
		return
	}

	supplyCounts := make(map[h3.Cell]int)
	for _, c := range couriers {
		cell, err := h3.LatLngToCell(h3.NewLatLng(c.Lat, c.Lng), 8)
		if err == nil {
			supplyCounts[cell]++
		}
	}

	newMultipliers := make(map[h3.Cell]float64)

	// Compute for all cells that have either supply or demand
	cells := make(map[h3.Cell]bool)
	for cell := range demandCounts {
		cells[cell] = true
	}
	for cell := range supplyCounts {
		cells[cell] = true
	}

	for cell := range cells {
		d := demandCounts[cell]
		s := supplyCounts[cell]

		mult := CalculateMultiplier(d, s)

		newMultipliers[cell] = mult

		// Emit Event
		payload, _ := json.Marshal(map[string]interface{}{
			"zone":       cell.String(),
			"multiplier": mult,
			"demand":     d,
			"supply":     s,
		})

		if e.publisher != nil {
			e.publisher.Publish(ctx, "surge.updated", payload)
		}

		if e.pool != nil {
			_, err := e.pool.Exec(ctx, `
				INSERT INTO surge_snapshots (h3, window_end_at, demand, supply, multiplier)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (h3, window_end_at) DO NOTHING
			`, cell.String(), now, d, s, mult)
			if err != nil {
				log.Printf("failed to record surge snapshot for %s: %v", cell.String(), err)
			}
		}
	}

	e.demandMu.Lock()
	e.multiplier = newMultipliers
	e.demandMu.Unlock()
}

// Zone is a surge zone with its hexagon outline, for drawing on a map.
type Zone struct {
	ID         string
	Multiplier float64
	Ring       [][2]float64 // closed ring of [lng, lat]
}

// Zones returns every zone whose multiplier is above 1.0, sorted by id.
func (e *Engine) Zones() []Zone {
	e.demandMu.Lock()
	hot := make(map[h3.Cell]float64)
	for cell, m := range e.multiplier {
		if m > 1.0 {
			hot[cell] = m
		}
	}
	e.demandMu.Unlock()

	e.ringMu.Lock()
	defer e.ringMu.Unlock()
	zones := make([]Zone, 0, len(hot))
	for cell, m := range hot {
		ring, ok := e.rings[cell]
		if !ok {
			boundary, err := cell.Boundary()
			if err != nil || len(boundary) == 0 {
				continue
			}
			ring = make([][2]float64, 0, len(boundary)+1)
			for _, p := range boundary {
				ring = append(ring, [2]float64{p.Lng, p.Lat})
			}
			ring = append(ring, ring[0])
			e.rings[cell] = ring
		}
		zones = append(zones, Zone{ID: cell.String(), Multiplier: m, Ring: ring})
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].ID < zones[j].ID })
	return zones
}

func (e *Engine) GetState() map[string]float64 {
	e.demandMu.Lock()
	defer e.demandMu.Unlock()

	state := make(map[string]float64, len(e.multiplier))
	for cell, mult := range e.multiplier {
		state[cell.String()] = mult
	}
	return state
}

func (e *Engine) Stop() {
	e.cancel()
	e.wg.Wait()
}
