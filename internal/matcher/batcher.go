package matcher

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"route/internal/claims"
	"route/internal/config"
	"route/internal/events"
	"route/internal/index"
	"route/internal/metrics"
	"route/internal/offers"
	"route/internal/surge"
)

type Batcher struct {
	pool      *pgxpool.Pool
	store     *index.Store
	claims    *claims.Manager
	lifecycle *offers.Lifecycle
	surge     *surge.Engine
	outbox    *events.Outbox

	weights  config.ScoringWeights
	windowMs time.Duration
	maxBatch int

	queue           chan string
	droppedTriggers atomic.Int64

	wg sync.WaitGroup

	// inflight limits concurrent batches; flushing tracks them for Stop.
	inflight chan struct{}
	flushing sync.WaitGroup

	// recording tracks the goroutines that write batch records.
	recording sync.WaitGroup

	// rates caches each courier's accept rate for a short while, see acceptRates.
	rateMu sync.Mutex
	rates  map[string]rateEntry
	ctx    context.Context
	cancel context.CancelFunc
}

func NewBatcher(pool *pgxpool.Pool, store *index.Store, claims *claims.Manager, lifecycle *offers.Lifecycle, outbox *events.Outbox, surge *surge.Engine) *Batcher {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Batcher{
		pool:      pool,
		store:     store,
		claims:    claims,
		lifecycle: lifecycle,
		outbox:    outbox,
		surge:     surge,
		weights:   config.Scoring,
		windowMs:  config.BatchWindow,
		maxBatch:  config.MaxBatchSize,
		queue:     make(chan string, 10000),
		rates:     make(map[string]rateEntry),
		inflight:  make(chan struct{}, config.MaxConcurrentBatches),
		ctx:       ctx,
		cancel:    cancel,
	}

	// A window of 0 means "solve each order as it arrives" (the greedy
	// baseline in the solver comparison). time.NewTicker panics on 0, so use
	// the smallest window that works.
	if b.windowMs < time.Millisecond {
		b.windowMs = time.Millisecond
	}

	b.wg.Add(1)
	go b.loop()
	return b
}

// SetWeights replaces the scoring weights. Call it before orders flow.
func (b *Batcher) SetWeights(w config.ScoringWeights) {
	b.weights = w
}

func (b *Batcher) SetLifecycle(l *offers.Lifecycle) {
	b.lifecycle = l
}

// Trigger enqueues an order for the next batch window. It takes no context:
// a batch always solves on a fresh background context, since the batch
// outlives whatever request (or sweep tick) triggered it.
func (b *Batcher) Trigger(orderID string) {
	select {
	case b.queue <- orderID:
	default:
		// The order is safe in Postgres as 'queued'; only this wake-up is
		// lost, and the sweep sends another. Count it, and log only the first
		// and every 10,000th: under overload one line per drop is a flood.
		metrics.TriggersDroppedTotal.Inc()
		if n := b.droppedTriggers.Add(1); n == 1 || n%10000 == 0 {
			log.Printf("batcher queue full: %d wake-ups dropped so far; the sweep will retry them", n)
		}
	}
}

func (b *Batcher) loop() {
	defer b.wg.Done()

	batch := make([]string, 0, b.maxBatch)
	var windowOpenAt time.Time
	ticker := time.NewTicker(b.windowMs)
	defer ticker.Stop()

	for {
		select {
		case <-b.ctx.Done():
			return
		case id := <-b.queue:
			if len(batch) == 0 {
				windowOpenAt = time.Now()
			}
			batch = append(batch, id)
			if len(batch) >= b.maxBatch {
				b.flush(batch, windowOpenAt)
				batch = make([]string, 0, b.maxBatch)
				ticker.Reset(b.windowMs)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				b.flush(batch, windowOpenAt)
				batch = make([]string, 0, b.maxBatch)
			}
		}
		metrics.QueueDepth.Set(float64(len(b.queue) + len(batch)))
	}
}

// flush solves a batch in the background. It waits when
// config.MaxConcurrentBatches are already running, which holds back the loop
// (and so the queue) instead of piling up work.
func (b *Batcher) flush(orderIDs []string, windowOpenAt time.Time) {
	metrics.BatchSize.Observe(float64(len(orderIDs)))
	b.inflight <- struct{}{}
	b.flushing.Add(1)
	go func() {
		defer func() {
			<-b.inflight
			b.flushing.Done()
		}()
		b.SolveBatch(context.Background(), orderIDs, windowOpenAt)
	}()
}

func (b *Batcher) Stop() {
	b.cancel()
	b.wg.Wait()
	b.flushing.Wait()
	b.recording.Wait()
}
