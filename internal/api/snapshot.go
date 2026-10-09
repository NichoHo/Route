package api

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"route/internal/config"
	"route/internal/index"
	"route/internal/metrics"
	"route/internal/surge"
)

// Snapshotter builds the dashboard's live view: couriers, open orders,
// pending offers and surge zones, as one JSON document.
type Snapshotter struct {
	pool  *pgxpool.Pool
	store *index.Store
	surge *surge.Engine
}

func NewSnapshotter(pool *pgxpool.Pool, store *index.Store, s *surge.Engine) *Snapshotter {
	return &Snapshotter{pool: pool, store: store, surge: s}
}

type orderState struct {
	ID         string  `json:"id"`
	PickupLng  float64 `json:"pickup_lng"`
	PickupLat  float64 `json:"pickup_lat"`
	DropoffLng float64 `json:"dropoff_lng"`
	DropoffLat float64 `json:"dropoff_lat"`
	Status     string  `json:"status"`
	CourierID  *string `json:"courier_id,omitempty"`
}

type offerState struct {
	OrderID   string `json:"order_id"`
	CourierID string `json:"courier_id"`
}

// Run publishes one snapshot per config.BroadcastInterval to the hub,
// regardless of how fast the underlying state changes, and skips the queries
// entirely while nobody is watching. It returns when ctx ends.
func (s *Snapshotter) Run(ctx context.Context, hub *Hub) {
	ticker := time.NewTicker(config.BroadcastInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if hub.Len() == 0 {
			continue
		}
		data, err := s.Snapshot(ctx)
		if err != nil {
			log.Printf("broadcast snapshot failed: %v", err)
			continue
		}
		hub.Broadcast(data)
	}
}

// Snapshot returns the current view as JSON.
func (s *Snapshotter) Snapshot(parent context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	states, err := s.store.GetAllActiveCouriers(ctx)
	if err != nil {
		return nil, err
	}
	if states == nil {
		states = make([]index.CourierState, 0)
	}

	// The status predicate matches idx_orders_active_recent so the query can
	// use that partial index.
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, ST_X(o.pickup), ST_Y(o.pickup), ST_X(o.dropoff), ST_Y(o.dropoff), o.status, a.courier_id
		FROM orders o
		LEFT JOIN assignments a ON o.id = a.order_id AND a.completed_at IS NULL
		WHERE o.status NOT IN ('completed', 'exhausted', 'cancelled') AND o.created_at > now() - interval '10 minutes'
	`)
	if err != nil {
		return nil, err
	}
	orders := make([]orderState, 0)
	for rows.Next() {
		var o orderState
		if err := rows.Scan(&o.ID, &o.PickupLng, &o.PickupLat, &o.DropoffLng, &o.DropoffLat, &o.Status, &o.CourierID); err != nil {
			rows.Close()
			return nil, err
		}
		orders = append(orders, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	offerRows, err := s.pool.Query(ctx, `SELECT order_id, courier_id FROM offers WHERE state = 'pending'`)
	if err != nil {
		return nil, err
	}
	pendingOffers := make([]offerState, 0)
	for offerRows.Next() {
		var o offerState
		if err := offerRows.Scan(&o.OrderID, &o.CourierID); err != nil {
			offerRows.Close()
			return nil, err
		}
		pendingOffers = append(pendingOffers, o)
	}
	offerRows.Close()
	if err := offerRows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"couriers":    states,
		"orders":      orders,
		"offers":      pendingOffers,
		"surge":       s.surge.GetState(),
		"surge_zones": surgeGeoJSON(s.surge.Zones()),
		"matches":     metrics.MatchesTotal(),
	})
}

// surgeGeoJSON renders surge zones as a GeoJSON FeatureCollection of hexagons
// for the dashboard's heatmap layer.
func surgeGeoJSON(zones []surge.Zone) map[string]interface{} {
	features := make([]map[string]interface{}, 0, len(zones))
	for _, z := range zones {
		features = append(features, map[string]interface{}{
			"type":       "Feature",
			"properties": map[string]interface{}{"zone": z.ID, "multiplier": z.Multiplier},
			"geometry": map[string]interface{}{
				"type":        "Polygon",
				"coordinates": [][][2]float64{z.Ring},
			},
		})
	}
	return map[string]interface{}{"type": "FeatureCollection", "features": features}
}
