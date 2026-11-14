package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type Envelope struct {
	RequestID     string   `json:"requestId,omitempty"`
	CorrelationID string   `json:"correlationId,omitempty"`
	Errors        []*Error `json:"errors"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil && status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func OK(w http.ResponseWriter, v any) { JSON(w, http.StatusOK, v) }

func Created(w http.ResponseWriter, v any) { JSON(w, http.StatusCreated, v) }

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

type ErrorReporter func(ctx context.Context, err error, r *http.Request)

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e := AsError(err)
	ctx := r.Context()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && e.Code == CodeInternal {
		e = E(CodeTimeout).Wrap(err)
	}
	log := LoggerFrom(ctx)
	if e.Status >= 500 {
		log.ErrorContext(ctx, "request failed", slog.String("code", string(e.Code)), slog.Any("error", err))
		if rep := reporterFrom(ctx); rep != nil {
			rep(ctx, err, r)
		}
	} else {
		log.DebugContext(ctx, "request rejected", slog.String("code", string(e.Code)), slog.String("error", err.Error()))
	}
	if e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "60")
		}
	}
	JSON(w, e.Status, Envelope{
		RequestID:     RequestID(ctx),
		CorrelationID: CorrelationID(ctx),
		Errors:        []*Error{e},
	})
}

type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

func (h HandlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}

func Handle[Req, Res any](fn func(ctx context.Context, req Req) (Res, error), opts ...BindOption) http.Handler {
	return HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		req, err := Bind[Req](r, opts...)
		if err != nil {
			return err
		}
		res, err := fn(r.Context(), req)
		if err != nil {
			return err
		}
		status := http.StatusOK
		if s, ok := any(res).(interface{ StatusCode() int }); ok {
			status = s.StatusCode()
		}
		JSON(w, status, res)
		return nil
	})
}
