package sse_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ahmedbaig/go-backend-http/sse"
)

func TestStreamFormatsAndHeartbeats(t *testing.T) {
	events := make(chan sse.Event, 2)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sse.LastEventID(r) != "5-0" {
			t.Errorf("last event id %q", sse.LastEventID(r))
		}
		s, err := sse.Start(w)
		if err != nil {
			t.Error(err)
			return
		}
		_ = sse.Serve(r.Context(), s, events, sse.Options{Heartbeat: 20 * time.Millisecond, Retry: 3 * time.Second, Done: done})
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Last-Event-ID", "5-0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events <- sse.Event{ID: "6-0", Type: "posts.created", Data: []byte("{\"a\":1}\nsecond")}

	sc := bufio.NewScanner(res.Body)
	var got []string
	deadline := time.After(2 * time.Second)
	for !strings.Contains(strings.Join(got, "\n"), ": ping") {
		line := make(chan string, 1)
		go func() {
			if sc.Scan() {
				line <- sc.Text()
			} else {
				close(line)
			}
		}()
		select {
		case l, ok := <-line:
			if !ok {
				t.Fatalf("stream ended early: %v", got)
			}
			got = append(got, l)
		case <-deadline:
			t.Fatalf("timed out: %v", got)
		}
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"retry: 3000", "id: 6-0", "event: posts.created", "data: {\"a\":1}", "data: second", ": ping"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in\n%s", want, all)
		}
	}
	close(done)
}

func TestServeStopsOnContext(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := sse.Start(rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sse.Serve(ctx, s, nil, sse.Options{}); err != nil {
		t.Fatal(err)
	}
}
