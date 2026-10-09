package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"route/internal/api/proto"
	"route/internal/auth"
	"route/internal/config"
)

type courierState int

const (
	stateIdle courierState = iota
	stateToPickup
	stateToDropoff
)

// idleSpeedFactor is how fast an idle courier drifts, as a share of its
// driving speed.
const idleSpeedFactor = 0.3

// orderLocation is what the order generator already knows about an order.
// Since routesim is the only order source, a courier can look up where to
// drive without the wire protocol carrying pickup and dropoff (the Offer
// message stays minimal).
type orderLocation struct {
	PickupLat, PickupLng   float64
	DropoffLat, DropoffLng float64
}

var (
	orderLocationsMu sync.Mutex
	orderLocations   = make(map[string]orderLocation)
)

// Courier is one simulated courier. All its fields except token are guarded
// by mu; token belongs to the session goroutine.
type Courier struct {
	ID string

	rng        *rand.Rand
	speed      float64 // m/s when driving
	acceptProb float64
	delayMs    [2]int
	flaky      bool
	flakyOffS  [2]float64
	flakyOnS   [2]float64

	mu           sync.Mutex
	lat, lng     float64
	theta        float64 // heading in radians clockwise from north
	state        courierState
	targetLat    float64
	targetLng    float64
	currentOrder string
	silentUntil  time.Time // flaky: no pings until then
	nextOutage   time.Time // flaky: next time it goes silent

	token  string
	client proto.CourierClient // the gRPC connection this courier uses
}

func newCourier(id string, p *Profile, rng *rand.Rand) *Courier {
	lat, lng := uniformPoint(rng)
	b := p.Behavior
	accept := b.AcceptMean + rng.NormFloat64()*b.AcceptStdDev
	c := &Courier{
		ID:         id,
		rng:        rng,
		speed:      p.SpeedMps,
		acceptProb: math.Min(1, math.Max(0, accept)),
		delayMs:    b.ResponseDelayMs,
		flaky:      rng.Float64() < b.FlakyFraction,
		flakyOffS:  b.FlakyOffS,
		flakyOnS:   b.FlakyOnS,
		lat:        lat,
		lng:        lng,
		theta:      rng.Float64() * 2 * math.Pi,
	}
	c.nextOutage = time.Now().Add(secs(uniformIn(rng, c.flakyOnS[0], c.flakyOnS[1])))
	return c
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// silent reports whether a flaky courier is in an outage at now, starting or
// ending outages as time passes. Non-flaky couriers are never silent.
func (c *Courier) silent(now time.Time) bool {
	if !c.flaky {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.silentUntil) {
		return true
	}
	if !now.Before(c.nextOutage) {
		c.silentUntil = now.Add(secs(uniformIn(c.rng, c.flakyOffS[0], c.flakyOffS[1])))
		c.nextOutage = c.silentUntil.Add(secs(uniformIn(c.rng, c.flakyOnS[0], c.flakyOnS[1])))
		return true
	}
	return false
}

// tick advances the courier by dt seconds and returns an order id exactly
// once, on the tick it arrives at its dropoff.
func (c *Courier) tick(dt float64) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case stateToPickup, stateToDropoff:
		if !c.moveToward(c.targetLat, c.targetLng, c.speed*dt) {
			return ""
		}
		if c.state == stateToPickup {
			orderLocationsMu.Lock()
			loc, ok := orderLocations[c.currentOrder]
			orderLocationsMu.Unlock()
			if !ok {
				// Shouldn't happen, but don't strand the courier.
				c.state, c.currentOrder = stateIdle, ""
				return ""
			}
			c.state = stateToDropoff
			c.targetLat, c.targetLng = loc.DropoffLat, loc.DropoffLng
			return ""
		}
		completed := c.currentOrder
		c.state, c.currentOrder = stateIdle, ""
		orderLocationsMu.Lock()
		delete(orderLocations, completed)
		orderLocationsMu.Unlock()
		return completed

	default:
		// Random walk with momentum: the heading drifts a little each tick
		// instead of jumping, and the courier bounces off the service area.
		c.theta += c.rng.NormFloat64() * 0.3 * math.Sqrt(dt)
		step := c.speed * idleSpeedFactor * dt
		c.lat += step * math.Cos(c.theta) / metersPerDegreeLat
		c.lng += step * math.Sin(c.theta) / metersPerDegreeLng(c.lat)
		if c.lat < config.JakartaMinLat || c.lat > config.JakartaMaxLat {
			c.theta = math.Pi - c.theta
		}
		if c.lng < config.JakartaMinLng || c.lng > config.JakartaMaxLng {
			c.theta = -c.theta
		}
		c.lat, c.lng = clampToBBox(c.lat, c.lng)
		return ""
	}
}

// moveToward steps stepM meters toward the target and reports whether it got
// there.
func (c *Courier) moveToward(targetLat, targetLng, stepM float64) bool {
	dy := (targetLat - c.lat) * metersPerDegreeLat
	dx := (targetLng - c.lng) * metersPerDegreeLng(c.lat)
	dist := math.Hypot(dx, dy)
	if dist <= stepM {
		c.lat, c.lng = targetLat, targetLng
		return true
	}
	c.theta = math.Atan2(dx, dy)
	c.lat += dy / dist * stepM / metersPerDegreeLat
	c.lng += dx / dist * stepM / metersPerDegreeLng(c.lat)
	return false
}

// heading is the compass heading in degrees, 0 to 360.
func (c *Courier) heading() float64 {
	deg := math.Mod(c.theta*180/math.Pi, 360)
	if deg < 0 {
		deg += 360
	}
	return deg
}

// responseDelay is how long this courier takes to answer an offer.
func (c *Courier) responseDelay() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	ms := c.delayMs[0]
	if c.delayMs[1] > c.delayMs[0] {
		ms += c.rng.Intn(c.delayMs[1] - c.delayMs[0] + 1)
	}
	return time.Duration(ms) * time.Millisecond
}

func (c *Courier) decideAccept() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rng.Float64() < c.acceptProb
}

// ---- session ----

// fetchCourierToken asks routed to issue the gRPC session token for one
// courier. The signing key stays on the server; the simulator only holds the
// API token.
func fetchCourierToken(httpAddr, apiToken string, client *http.Client, courierID string) (string, error) {
	req, err := http.NewRequest("POST", httpAddr+"/couriers/"+courierID+"/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request rejected: %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("bad token response: %v", err)
	}
	return out.Token, nil
}

// run keeps the courier connected until ctx ends. connectOnce owns one
// stream's lifetime, so its ticker and receive goroutine are always cleaned
// up before the next attempt.
func (c *Courier) run(ctx context.Context, env *simEnv, pingEvery time.Duration) {
	seq := int64(0)
	for ctx.Err() == nil {
		c.connectOnce(ctx, env, pingEvery, &seq)
		// Always back off: a session that ends at once (rejected credentials,
		// server at capacity) must not turn into a hot reconnect loop.
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Courier) connectOnce(parent context.Context, env *simEnv, pingEvery time.Duration, seq *int64) {
	if c.token == "" {
		tok, err := fetchCourierToken(env.httpAddr, env.apiToken, env.httpClient, c.ID)
		if err != nil {
			env.stats.tokenErrors.Add(1)
			log.Printf("token error for %s: %v", c.ID, err)
			return
		}
		c.token = tok
	}
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(parent, auth.CourierMetadata(c.ID, c.token)))
	defer cancel() // ends the receive goroutine if we leave on a send error

	stream, err := c.client.Session(ctx)
	if err != nil {
		log.Printf("session error for %s: %v", c.ID, err)
		return
	}
	env.stats.sessions.Add(1)

	var sendMu sync.Mutex
	send := func(msg *proto.CourierMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	done := make(chan struct{})
	var recvErr error
	go func() {
		defer close(done)
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr = err
				return
			}
			if offer := msg.GetOffer(); offer != nil {
				go c.respondToOffer(ctx, send, offer)
			} else if outcome := msg.GetOfferOutcome(); outcome != nil {
				c.handleOfferOutcome(outcome)
			}
		}
	}()

	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			if status.Code(recvErr) == codes.Unauthenticated {
				c.token = "" // fetch a fresh token on the next attempt
			}
			env.stats.reconnects.Add(1)
			return
		case now := <-ticker.C:
			dt := now.Sub(last).Seconds()
			last = now
			if completed := c.tick(dt); completed != "" {
				send(&proto.CourierMessage{Payload: &proto.CourierMessage_CompletionReport{
					CompletionReport: &proto.CompletionReport{OrderId: completed},
				}})
			}
			if c.silent(now) {
				continue // a flaky courier's GPS has dropped out
			}
			c.mu.Lock()
			lat, lng, heading := c.lat, c.lng, c.heading()
			c.mu.Unlock()
			err := send(&proto.CourierMessage{Payload: &proto.CourierMessage_Ping{Ping: &proto.Ping{
				CourierId: c.ID, Lat: lat, Lng: lng, Heading: heading,
				ClientTs: now.UnixMilli(), Seq: *seq,
			}}})
			if err != nil {
				return
			}
			*seq++
		}
	}
}

func (c *Courier) respondToOffer(ctx context.Context, send func(*proto.CourierMessage) error, offer *proto.Offer) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(c.responseDelay()):
	}
	accept := c.decideAccept()
	if accept {
		// Record which order we are accepting *before* sending: the server's
		// confirming OfferOutcome can arrive before send returns, and
		// handleOfferOutcome needs this to start the drive to pickup. A
		// rejected accept clears it there.
		c.mu.Lock()
		c.currentOrder = offer.OrderId
		c.mu.Unlock()
	}
	if err := send(&proto.CourierMessage{Payload: &proto.CourierMessage_OfferResponse{
		OfferResponse: &proto.OfferResponse{OfferId: offer.OfferId, Accept: accept},
	}}); err != nil && accept {
		c.mu.Lock()
		c.currentOrder = ""
		c.mu.Unlock()
	}
}

func (c *Courier) handleOfferOutcome(outcome *proto.OfferOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !outcome.Success {
		c.currentOrder = ""
		return
	}
	if c.currentOrder == "" {
		return // a declined offer's confirmation
	}
	orderLocationsMu.Lock()
	loc, ok := orderLocations[c.currentOrder]
	orderLocationsMu.Unlock()
	if !ok {
		return
	}
	c.state = stateToPickup
	c.targetLat, c.targetLng = loc.PickupLat, loc.PickupLng
}
