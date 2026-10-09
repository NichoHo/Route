package ingest

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"route/internal/api/proto"
)

// historyRow pairs a ping with the server's receive time: history is stamped
// with server time, never the client's clock (ROUTE_SPEC.md ADR 007).
type historyRow struct {
	ping *proto.Ping
	ts   time.Time
}

type HistoryWriter struct {
	pool *pgxpool.Pool
	ch   chan historyRow
}

func NewHistoryWriter(pool *pgxpool.Pool, bufferSize int) *HistoryWriter {
	hw := &HistoryWriter{
		pool: pool,
		ch:   make(chan historyRow, bufferSize),
	}
	go hw.startWorker()
	return hw
}

func (hw *HistoryWriter) startWorker() {
	var batch []historyRow
	ticker := time.NewTicker(1 * time.Second)

	flush := func() {
		if len(batch) == 0 {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var rows [][]interface{}
		for _, r := range batch {
			rows = append(rows, []interface{}{
				r.ping.CourierId,
				r.ts,
				r.ping.Lng,
				r.ping.Lat,
				r.ping.Heading,
				r.ping.Seq,
			})
		}

		// pgx.Batch (not CopyFrom) so each row's geometry can go through
		// ST_SetSRID/ST_MakePoint.
		b := &pgx.Batch{}
		query := `
			INSERT INTO courier_positions (courier_id, ts, geom, heading, seq)
			VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326), $5, $6)
		`
		for _, row := range rows {
			b.Queue(query, row...)
		}

		br := hw.pool.SendBatch(ctx, b)
		// BatchResults must be drained with one Exec per queued statement,
		// not just the first, or the rest are left unread on the wire.
		for i := 0; i < b.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				log.Printf("failed to write history row %d/%d: %v", i+1, b.Len(), err)
			}
		}
		if err := br.Close(); err != nil {
			log.Printf("failed to close history batch: %v", err)
		}

		batch = batch[:0]
	}

	for {
		select {
		case ping, ok := <-hw.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ping)
			if len(batch) >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (hw *HistoryWriter) Write(ping *proto.Ping) {
	select {
	case hw.ch <- historyRow{ping: ping, ts: time.Now()}:
	default:
		// Drop historical record rather than applying backpressure to live ingestion
	}
}

func (hw *HistoryWriter) Close() {
	close(hw.ch)
}
