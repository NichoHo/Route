package claims

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Manager struct {
	client *redis.Client
}

func NewManager(client *redis.Client) *Manager {
	return &Manager{client: client}
}

func (m *Manager) AcquireOfferClaim(ctx context.Context, courierID, offerID string, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("route:claim:%s", courierID)
	return m.client.SetNX(ctx, key, offerID, ttl).Result()
}

var releaseScript = redis.NewScript(`
	if redis.call("get", KEYS[1]) == ARGV[1] then
		return redis.call("del", KEYS[1])
	else
		return 0
	end
`)

func (m *Manager) ReleaseClaim(ctx context.Context, courierID, offerID string) error {
	key := fmt.Sprintf("route:claim:%s", courierID)
	_, err := releaseScript.Run(ctx, m.client, []string{key}, offerID).Result()
	return err
}

// RestoreResult says what Restore found.
type RestoreResult int

const (
	ClaimMismatch RestoreResult = -1 // held under a different value; left alone
	ClaimPresent  RestoreResult = 0  // already held under the wanted value
	ClaimRestored RestoreResult = 1  // was absent, or held under replaceable; now set
)

var restoreScript = redis.NewScript(`
	local cur = redis.call("get", KEYS[1])
	if not cur then
		redis.call("set", KEYS[1], ARGV[1], "PX", ARGV[3])
		return 1
	end
	if cur == ARGV[1] then
		return 0
	end
	if ARGV[2] ~= "" and cur == ARGV[2] then
		redis.call("set", KEYS[1], ARGV[1], "PX", ARGV[3])
		return 1
	end
	return -1
`)

// Restore makes the courier's claim hold value, atomically: it sets the claim
// if it is absent, replaces it if it currently holds replaceable (the offer id
// being converted to an assignment id), and otherwise leaves it alone.
//
// Postgres is the durable record and Redis the hot-path authority, so this is
// how Redis is brought back in line after a restart or a lost reply: on
// accept (offer claim -> assignment claim) and in reconciliation.
func (m *Manager) Restore(ctx context.Context, courierID, value, replaceable string, ttl time.Duration) (RestoreResult, error) {
	key := fmt.Sprintf("route:claim:%s", courierID)
	res, err := restoreScript.Run(ctx, m.client, []string{key}, value, replaceable, ttl.Milliseconds()).Int64()
	if err != nil {
		return ClaimMismatch, err
	}
	return RestoreResult(res), nil
}

// BulkCheck reports which of courierIDs currently hold a claim (offer or
// assignment). Used by read-only endpoints that want a coarse
// available/claimed status without resolving the exact offer or assignment
// id each claim holds.
func (m *Manager) BulkCheck(ctx context.Context, courierIDs []string) map[string]bool {
	claimed := make(map[string]bool, len(courierIDs))
	if len(courierIDs) == 0 {
		return claimed
	}
	keys := make([]string, len(courierIDs))
	for i, id := range courierIDs {
		keys[i] = fmt.Sprintf("route:claim:%s", id)
	}
	vals, err := m.client.MGet(ctx, keys...).Result()
	if err != nil {
		return claimed
	}
	for i, v := range vals {
		if v != nil {
			claimed[courierIDs[i]] = true
		}
	}
	return claimed
}
