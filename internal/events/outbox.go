package events

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

// relayBatchSize is how many events one relay pass publishes. The old limit of
// 100 per 100ms tick capped the relay near 1,000 events a second, below what a
// 500 orders-a-second load produces.
const relayBatchSize = 1000

type Outbox struct {
	pool   *pgxpool.Pool
	client *kgo.Client

	ctx    context.Context
	cancel context.CancelFunc
}

func NewOutbox(pool *pgxpool.Pool, brokers []string) (*Outbox, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		// franz-go defaults this to false regardless of the broker's own
		// auto-create setting: the client tells the broker not to create
		// missing topics unless this is set. Without it, every produce to
		// a not-yet-created topic (i.e. every topic, the first time)
		// fails with UNKNOWN_TOPIC_OR_PARTITION.
		kgo.AllowAutoTopicCreation(),
	)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	ob := &Outbox{
		pool:   pool,
		client: cl,
		ctx:    ctx,
		cancel: cancel,
	}

	go ob.relayLoop()

	return ob, nil
}

// Insert adds an event to the outbox inside an existing Postgres transaction.
func (o *Outbox) Insert(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error {
	id := uuid.New().String()
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, topic, payload, created_at)
		VALUES ($1, $2, $3, NOW())
	`, id, topic, payload)
	return err
}

// Publish adds an event to the outbox outside a transaction.
func (o *Outbox) Publish(ctx context.Context, topic string, payload []byte) error {
	id := uuid.New().String()
	_, err := o.pool.Exec(ctx, `
		INSERT INTO outbox (id, topic, payload, created_at)
		VALUES ($1, $2, $3, NOW())
	`, id, topic, payload)
	return err
}

func (o *Outbox) relayLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			// A full batch means there is probably more: keep going until
			// the backlog is gone, instead of waiting a tick between batches.
			for o.relayBatch() == relayBatchSize {
			}
		}
	}
}

// relayBatch publishes up to relayBatchSize events and returns how many were
// published and removed from the outbox.
func (o *Outbox) relayBatch() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return 0
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, topic, payload
		FROM outbox
		ORDER BY created_at ASC
		LIMIT 1000
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return 0
	}

	type row struct{ id, topic string }
	var rowsByRecord []row
	var records []*kgo.Record
	for rows.Next() {
		var r row
		var payload []byte
		if err := rows.Scan(&r.id, &r.topic, &payload); err != nil {
			log.Printf("outbox scan error: %v", err)
			continue
		}
		records = append(records, &kgo.Record{Topic: r.topic, Value: payload})
		rowsByRecord = append(rowsByRecord, r)
	}
	rows.Close()

	if len(records) == 0 {
		return 0
	}

	// One batched produce instead of one round trip per row. ProduceResults
	// can arrive in a different order than records were given (completions
	// race across partitions), so match back to the row by the *Record
	// pointer kgo hands back rather than by index.
	idByRecord := make(map[*kgo.Record]string, len(records))
	for i, rec := range records {
		idByRecord[rec] = rowsByRecord[i].id
	}

	results := o.client.ProduceSync(ctx, records...)
	var toDelete []string
	for _, res := range results {
		if res.Err != nil {
			log.Printf("produce error: %v", res.Err)
			continue
		}
		if id, ok := idByRecord[res.Record]; ok {
			toDelete = append(toDelete, id)
		}
	}

	if len(toDelete) > 0 {
		_, err = tx.Exec(ctx, `
			DELETE FROM outbox
			WHERE id = ANY($1)
		`, toDelete)
		if err != nil {
			log.Printf("failed to delete published outbox: %v", err)
		}
	}

	tx.Commit(ctx)
	// Count what was published, not what was read: if Redpanda is down this
	// is 0, and the caller stops draining instead of spinning on a full batch.
	return len(toDelete)
}

func (o *Outbox) Stop() {
	o.cancel()
	o.client.Close()
}
