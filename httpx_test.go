package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ahmedbaig/go-backend-logging"
	"github.com/ahmedbaig/go-backend-monitoring"
)

type createItem struct {
	ID    string   `path:"id"`
	Name  string   `json:"name" validate:"required,min=2"`
	Email string   `json:"email" validate:"omitempty,email"`
	Limit int      `query:"limit" validate:"omitempty,max=100"`
	Tags  []string `query:"tag"`
}

type itemResp struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Limit int      `json:"limit"`
	Tags  []string `json:"tags"`
}

func (itemResp) StatusCode() int { return http.StatusCreated }

func newApp(t *testing.T, logBuf *bytes.Buffer) *App {
	t.Helper()
	var log *slog.Logger
	if logBuf != nil {
		log = slog.New(logging.NewContextHandler(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	} else {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	app := New(Options{
		Name:   "svc",
		Logger: log,
		Prober: monitoring.NewProber("svc", monitoring.WithoutScheduling()),
	})
	v1 := app.Group("/api/v1")
	v1.Post("/items/{id}", Handle(func(ctx context.Context, in createItem) (itemResp, error) {
		if in.Name == "taken" {
			return itemResp{}, E(CodeConflict).WithMessage("name taken")
		}
		return itemResp{ID: in.ID, Name: in.Name, Limit: in.Limit, Tags: in.Tags}, nil
	}))
	v1.Get("/boom", HandlerFunc(func(http.ResponseWriter, *http.Request) error { panic("kaboom") }))
	v1.Get("/fail", HandlerFunc(func(http.ResponseWriter, *http.Request) error { return errors.New("db exploded") }))
	v1.Get("/ctx", HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		OK(w, map[string]string{"rid": RequestID(r.Context()), "cid": CorrelationID(r.Context())})
		return nil
	}))
	return app
}

func do(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) Envelope {
	t.Helper()
	var e Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an envelope: %s", rec.Body)
	}
	return e
}

func TestBindAndRespond(t *testing.T) {
	h := newApp(t, nil).Handler()
	rec := do(t, h, "POST", "/api/v1/items/abc?limit=5&tag=a,b&tag=c", `{"name":"widget"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var got itemResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID != "abc" || got.Name != "widget" || got.Limit != 5 || strings.Join(got.Tags, "") != "abc" {
		t.Fatalf("got %+v", got)
	}
}

func TestValidationEnvelope(t *testing.T) {
	h := newApp(t, nil).Handler()
	rec := do(t, h, "POST", "/api/v1/items/abc?limit=500", `{"name":"x","email":"nope"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	e := envelope(t, rec)
	if e.RequestID == "" || e.CorrelationID == "" || len(e.Errors) != 1 || e.Errors[0].Code != CodeValidation {
		t.Fatalf("envelope %+v", e)
	}
	fields := map[string]string{}
	for _, f := range e.Errors[0].Fields {
		fields[f.Field] = f.Rule
	}
	if fields["name"] != "min" || fields["email"] != "email" || fields["limit"] != "max" {
		t.Fatalf("fields %+v", e.Errors[0].Fields)
	}
}

func TestBodyErrors(t *testing.T) {
	h := newApp(t, nil).Handler()
	cases := []struct {
		body string
		code Code
	}{
		{`{"name":`, CodeInvalidJSON},
		{`{"name":"ok","extra":1}`, CodeValidation},
		{`{"name":5}`, CodeValidation},
		{`{"name":"ok"}{"name":"again"}`, CodeInvalidJSON},
	}
	for _, c := range cases {
		rec := do(t, h, "POST", "/api/v1/items/x", c.body, nil)
		if e := envelope(t, rec); e.Errors[0].Code != c.code {
			t.Errorf("%s: got %s want %s", c.body, e.Errors[0].Code, c.code)
		}
	}
	rec := do(t, h, "POST", "/api/v1/items/x", `{"name":"ok"}`, map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", rec.Code)
	}
}

func TestDomainErrorAndUnknownErrorIs500(t *testing.T) {
	var logs bytes.Buffer
	h := newApp(t, &logs).Handler()
	rec := do(t, h, "POST", "/api/v1/items/x", `{"name":"taken"}`, nil)
	if rec.Code != http.StatusConflict || envelope(t, rec).Errors[0].Message != "name taken" {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/v1/fail", "", nil)
	e := envelope(t, rec)
	if rec.Code != http.StatusInternalServerError || e.Errors[0].Code != CodeInternal {
		t.Fatalf("unknown error: %d %+v", rec.Code, e)
	}
	if strings.Contains(rec.Body.String(), "db exploded") {
		t.Fatal("internal error detail leaked to client")
	}
	if !strings.Contains(logs.String(), "db exploded") {
		t.Fatal("internal error not logged")
	}
}

func TestPanicRecovered(t *testing.T) {
	var logs bytes.Buffer
	h := newApp(t, &logs).Handler()
	rec := do(t, h, "GET", "/api/v1/boom", "", nil)
	if rec.Code != 500 || !strings.Contains(logs.String(), "kaboom") {
		t.Fatalf("%d %s", rec.Code, logs.String())
	}
}

func TestRequestIDs(t *testing.T) {
	h := newApp(t, nil).Handler()
	rec := do(t, h, "GET", "/api/v1/ctx", "", map[string]string{HeaderCorrelationID: "corr-123"})
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["cid"] != "corr-123" || rec.Header().Get(HeaderCorrelationID) != "corr-123" {
		t.Fatalf("correlation not propagated: %v", got)
	}
	if got["rid"] == "" || rec.Header().Get(HeaderRequestID) != got["rid"] {
		t.Fatalf("request id: %v", got)
	}
	rec = do(t, h, "GET", "/api/v1/ctx", "", map[string]string{HeaderCorrelationID: "bad value\nwith newline"})
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if strings.Contains(got["cid"], "bad") {
		t.Fatal("unsafe incoming id accepted")
	}
}

func TestBuiltinRoutesAndNotFound(t *testing.T) {
	h := newApp(t, nil).Handler()
	for _, p := range []string{"/healthz", "/healthp", "/metrics"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != 200 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	rec := do(t, h, "GET", "/nope", "", nil)
	if rec.Code != 404 || envelope(t, rec).Errors[0].Code != CodeRouteNotFound {
		t.Fatalf("404: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}

func TestAccessLogHasRoute(t *testing.T) {
	var logs bytes.Buffer
	h := newApp(t, &logs).Handler()
	do(t, h, "POST", "/api/v1/items/z", `{"name":"widget"}`, nil)
	if !strings.Contains(logs.String(), `"route":"POST /api/v1/items/{id}"`) || !strings.Contains(logs.String(), `"requestId"`) {
		t.Fatalf("access log: %s", logs.String())
	}
	logs.Reset()
	do(t, h, "GET", "/healthz", "", nil)
	if strings.Contains(logs.String(), "/healthz") {
		t.Fatal("health check should not be access-logged")
	}
}

func TestRateLimitOnUnmatched(t *testing.T) {
	app := New(Options{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Prober:    monitoring.NewProber("svc", monitoring.WithoutScheduling()),
		RateLimit: &RateLimitOptions{Requests: 2, Window: time.Minute},
	})
	h := app.Handler()
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, do(t, h, "GET", "/scan", "", nil).Code)
	}
	if codes[0] != 404 || codes[1] != 404 || codes[2] != 429 {
		t.Fatalf("codes %v", codes)
	}
}

func TestCORS(t *testing.T) {
	h := CORS(CORSOptions{AllowedOrigins: []string{"https://app.example"}, AllowCredentials: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := do(t, h, "OPTIONS", "/", "", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST"})
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("preflight %d %v", rec.Code, rec.Header())
	}
	rec = do(t, h, "OPTIONS", "/", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if rec.Code != 403 {
		t.Fatalf("disallowed preflight %d", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.2")
	trusted := append([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, LoopbackOnly...)
	if got := ClientIP(r, trusted); got != "203.0.113.9" {
		t.Fatalf("trusted chain: %s", got)
	}
	if got := ClientIP(r, nil); got != "127.0.0.1" {
		t.Fatalf("untrusted must ignore XFF: %s", got)
	}
}

func TestPropagatingTransport(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Get(HeaderCorrelationID) }))
	defer srv.Close()
	ctx := WithIDs(context.Background(), "r", "c-77")
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	res, err := NewClient(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if seen != "c-77" {
		t.Fatalf("seen %q", seen)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	app := newApp(t, nil)
	stopped := false
	app.OnShutdown(func(context.Context) error { stopped = true; return nil })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()
	url := "http://" + ln.Addr().String() + "/healthz"
	for i := 0; i < 50; i++ {
		if res, err := http.Get(url); err == nil {
			res.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}
	if !stopped {
		t.Fatal("shutdown hook not run")
	}
}

func TestCustomErrorRegistration(t *testing.T) {
	Register("E422-1", Definition{Status: 422, Message: "Order cannot be fulfilled"})
	e := E("E422-1")
	if e.Status != 422 || !errors.Is(e.Wrap(io.EOF), E("E422-1")) || !errors.Is(e.Wrap(io.EOF), io.EOF) {
		t.Fatalf("%+v", e)
	}
}
