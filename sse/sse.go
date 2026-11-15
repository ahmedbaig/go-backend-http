package sse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID    string
	Type  string
	Data  []byte
	Retry time.Duration
}

type Stream struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

var ErrUnsupported = errors.New("sse: response writer cannot flush")

func Start(w http.ResponseWriter) (*Stream, error) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	_ = rc.SetWriteDeadline(time.Time{})
	return &Stream{w: w, rc: rc}, nil
}

func (s *Stream) Send(e Event) error {
	var b strings.Builder
	if e.ID != "" {
		b.WriteString("id: " + clean(e.ID) + "\n")
	}
	if e.Type != "" {
		b.WriteString("event: " + clean(e.Type) + "\n")
	}
	if e.Retry > 0 {
		fmt.Fprintf(&b, "retry: %d\n", e.Retry.Milliseconds())
	}
	for _, line := range strings.Split(string(e.Data), "\n") {
		b.WriteString("data: " + strings.TrimSuffix(line, "\r") + "\n")
	}
	b.WriteString("\n")
	return s.write(b.String())
}

func (s *Stream) Comment(text string) error {
	return s.write(": " + clean(text) + "\n\n")
}

func (s *Stream) write(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write([]byte(p)); err != nil {
		return err
	}
	return s.rc.Flush()
}

func clean(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func LastEventID(r *http.Request) string {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return v
	}
	return r.URL.Query().Get("lastEventId")
}

type Options struct {
	Heartbeat time.Duration
	Retry     time.Duration
	Done      <-chan struct{}
}

func Serve(ctx context.Context, s *Stream, events <-chan Event, o Options) error {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.Retry > 0 {
		if err := s.write(fmt.Sprintf("retry: %d\n\n", o.Retry.Milliseconds())); err != nil {
			return err
		}
	}
	tick := time.NewTicker(o.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-o.Done:
			return nil
		case <-tick.C:
			if err := s.Comment("ping"); err != nil {
				return err
			}
		case e, ok := <-events:
			if !ok {
				return nil
			}
			if err := s.Send(e); err != nil {
				return err
			}
		}
	}
}
