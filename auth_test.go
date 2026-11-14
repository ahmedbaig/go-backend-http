package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmedbaig/go-backend-monitoring"
	"github.com/ahmedbaig/go-backend-types"
)

func TestAuthMiddleware(t *testing.T) {
	app := New(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Prober: monitoring.NewProber("svc", monitoring.WithoutScheduling())})
	g := app.Group("/api", RequireUser())
	g.Get("/me", Handle(func(ctx context.Context, _ types.Empty) (types.User, error) { return User(ctx) }, NoBody()))
	g.Get("/admin", HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { NoContent(w); return nil }), RequireRole("admin"))
	g.Get("/beta", HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { NoContent(w); return nil }), RequireFeature("beta"))
	h := app.Handler()

	call := func(path, user string) int {
		req := httptest.NewRequest("GET", path, nil)
		if user != "" {
			req.Header.Set(types.HeaderUser, user)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cases := []struct {
		path, user string
		want       int
	}{
		{"/api/me", "", 401},
		{"/api/me", "{bad", 401},
		{"/api/me", `{"id":1}`, 200},
		{"/api/admin", `{"id":1,"roles":["user"]}`, 403},
		{"/api/admin", `{"id":1,"roles":["admin"]}`, 204},
		{"/api/beta", `{"id":1}`, 403},
		{"/api/beta", `{"id":1,"features":["beta"]}`, 204},
	}
	for _, c := range cases {
		if got := call(c.path, c.user); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.path, c.user, got, c.want)
		}
	}
	if _, err := User(context.Background()); AsError(err).Code != CodeUnauthenticated {
		t.Fatal("User without session")
	}
	rid, cid := IDs(WithIDs(context.Background(), "r", "c"))
	if rid != "r" || cid != "c" {
		t.Fatal("IDs")
	}
}
