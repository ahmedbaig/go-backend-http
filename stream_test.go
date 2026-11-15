package httpx_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ahmedbaig/go-backend-http"
)

func TestTimeoutSkipsEventStreams(t *testing.T) {
	var hasDeadline bool
	h := httpx.Timeout(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
	}))
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if hasDeadline {
		t.Fatal("event stream got a deadline")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !hasDeadline {
		t.Fatal("normal request lost its deadline")
	}
}

func TestValidate(t *testing.T) {
	type in struct {
		Title string `json:"title" validate:"required,min=3"`
	}
	err := httpx.Validate(in{Title: "a"})
	var e *httpx.Error
	if !errors.As(err, &e) || e.Code != httpx.CodeValidation || len(e.Fields) != 1 || e.Fields[0].Field != "title" {
		t.Fatalf("got %#v", err)
	}
	if httpx.Validate(in{Title: "abc"}) != nil {
		t.Fatal("valid input rejected")
	}
}

func TestDrainingClosesOnShutdown(t *testing.T) {
	app := httpx.New(httpx.Options{})
	srv := app.Server("127.0.0.1:0")
	select {
	case <-app.Draining():
		t.Fatal("draining before shutdown")
	default:
	}
	_ = srv.Shutdown(t.Context())
	select {
	case <-app.Draining():
	case <-time.After(time.Second):
		t.Fatal("draining not closed")
	}
}
