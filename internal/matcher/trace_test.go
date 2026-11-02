package matcher_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"route/internal/api"
)

// ROUTE_SPEC.md 9.2: one trace follows an order from POST /orders through the
// claim and offer to the accept. The batch it was solved in is a separate
// trace (it serves many orders) and links to it.
func TestOrderTraceRunsToAccept(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	h := newHarness(t, "", 5*time.Second)
	h.addCourier("trace-1", accepts, -6.2000, 106.8166, 0)
	h.waitIndexed(1)

	orderAPI := api.NewOrderAPI(h.pool, h.batcher, &mockOutbox{}, h.surge)
	req := httptest.NewRequest("POST", "/orders", strings.NewReader(
		`{"pickup_lat":-6.2001,"pickup_lng":106.8167,"dropoff_lat":-6.21,"dropoff_lng":106.82,"priority":1}`))
	req.Header.Set("Idempotency-Key", "trace-order-1")
	w := httptest.NewRecorder()
	orderAPI.ServeHTTP(w, req)
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 201 || out.ID == "" {
		t.Fatalf("POST /orders: %d %s", w.Code, w.Body.String())
	}
	h.eventually(10*time.Second, "order assigned", func() bool { return h.orderStatus(out.ID) == "assigned" })
	// AcceptOffer ends after the commit the status check can already see.
	h.eventually(2*time.Second, "AcceptOffer span ended", func() bool {
		for _, s := range rec.Ended() {
			if s.Name() == "AcceptOffer" {
				return true
			}
		}
		return false
	})

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		spans[s.Name()] = s
	}
	root, ok := spans["POST /orders"]
	if !ok {
		t.Fatal("no POST /orders span")
	}
	traceID := root.SpanContext().TraceID()
	for _, name := range []string{"DispatchOrder", "AcquireOfferClaim", "CreateOffer", "AcceptOffer"} {
		s, ok := spans[name]
		if !ok {
			t.Errorf("no %s span", name)
			continue
		}
		if s.SpanContext().TraceID() != traceID {
			t.Errorf("%s is in trace %s, want the order's trace %s", name, s.SpanContext().TraceID(), traceID)
		}
	}

	batch, ok := spans["SolveBatch"]
	if !ok {
		t.Fatal("no SolveBatch span")
	}
	linked := false
	for _, l := range batch.Links() {
		linked = linked || l.SpanContext.TraceID() == traceID
	}
	if !linked {
		t.Error("SolveBatch does not link to the order's trace")
	}
	if spans["candidates"] == nil || spans["candidates"].Parent().SpanID() != batch.SpanContext().SpanID() {
		t.Error("candidate gathering is not a child of SolveBatch")
	}
}
