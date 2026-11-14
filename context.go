package httpx

import (
	"context"
	"log/slog"
	"net/http"
)

const (
	HeaderCorrelationID = "X-Correlation-ID"
	HeaderRequestID     = "X-Request-ID"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyCorrelationID
	keyLogger
	keyReporter
)

func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

func CorrelationID(ctx context.Context) string {
	s, _ := ctx.Value(keyCorrelationID).(string)
	return s
}

func WithIDs(ctx context.Context, requestID, correlationID string) context.Context {
	ctx = context.WithValue(ctx, keyRequestID, requestID)
	return context.WithValue(ctx, keyCorrelationID, correlationID)
}

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, keyLogger, l)
}

func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(keyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

func withReporter(ctx context.Context, r ErrorReporter) context.Context {
	return context.WithValue(ctx, keyReporter, r)
}

func reporterFrom(ctx context.Context) ErrorReporter {
	r, _ := ctx.Value(keyReporter).(ErrorReporter)
	return r
}

type propagatingTransport struct {
	next http.RoundTripper
}

func PropagatingTransport(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return propagatingTransport{next: next}
}

func (t propagatingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cid := CorrelationID(r.Context())
	if cid == "" || r.Header.Get(HeaderCorrelationID) != "" {
		return t.next.RoundTrip(r)
	}
	c := r.Clone(r.Context())
	c.Header.Set(HeaderCorrelationID, cid)
	return t.next.RoundTrip(c)
}

func NewClient(next http.RoundTripper) *http.Client {
	return &http.Client{Transport: PropagatingTransport(next)}
}
