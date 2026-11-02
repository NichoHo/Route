// Package tracing carries an order's trace across the gap between the HTTP
// request that created it and the batch and courier stream that serve it.
// The W3C traceparent is stored on the order row; later steps continue the
// order's trace from it.
package tracing

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var tc = propagation.TraceContext{}

// Traceparent returns the W3C traceparent of the span in ctx, or "" if none.
func Traceparent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	tc.Inject(ctx, c)
	return c["traceparent"]
}

// SpanContext parses a stored traceparent. It returns an invalid
// SpanContext for "" or a malformed value.
func SpanContext(traceparent string) trace.SpanContext {
	ctx := tc.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent})
	return trace.SpanContextFromContext(ctx)
}

// Continue returns ctx with the stored trace as its remote parent, so a span
// started from it joins that trace. With no valid stored trace, ctx is
// returned unchanged.
func Continue(ctx context.Context, sc trace.SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
