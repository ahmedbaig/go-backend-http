package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ahmedbaig/go-backend-monitoring"
)

type Options struct {
	Name            string
	Environment     string
	Logger          *slog.Logger
	Prober          *monitoring.Prober
	Metrics         *monitoring.Metrics
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	MaxBodyBytes    int64
	CORS            *CORSOptions
	RateLimit       *RateLimitOptions
	Security        SecurityHeadersOptions
	TrustedProxies  []netip.Prefix
	ErrorReporter   ErrorReporter
	Middleware      []Middleware
	StaticDir       string
	StaticPrefix    string
}

type App struct {
	opts      Options
	mux       *http.ServeMux
	log       *slog.Logger
	mu        sync.Mutex
	onStop    []func(context.Context) error
	handler   http.Handler
	buildOnce sync.Once
	drain     chan struct{}
	drainOnce sync.Once
}

func New(o Options) *App {
	if o.Name == "" {
		o.Name = "service"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Prober == nil {
		o.Prober = monitoring.NewProber(o.Name)
	}
	if o.Metrics == nil {
		o.Metrics = monitoring.NewMetrics(monitoring.MetricsOptions{Module: o.Name})
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 15 * time.Second
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 30 * time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 120 * time.Second
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 10 << 20
	}
	if o.TrustedProxies == nil {
		o.TrustedProxies = LoopbackOnly
	}
	return &App{opts: o, mux: http.NewServeMux(), log: o.Logger.With(slog.String("component", "http")), drain: make(chan struct{})}
}

func (a *App) Draining() <-chan struct{} { return a.drain }

func (a *App) startDrain() { a.drainOnce.Do(func() { close(a.drain) }) }

func (a *App) Mux() *http.ServeMux { return a.mux }

func (a *App) Prober() *monitoring.Prober { return a.opts.Prober }

func (a *App) Metrics() *monitoring.Metrics { return a.opts.Metrics }

func (a *App) Logger() *slog.Logger { return a.opts.Logger }

func (a *App) Handle(pattern string, h http.Handler) { a.mux.Handle(pattern, h) }

func (a *App) HandleFunc(pattern string, h HandlerFunc) { a.mux.Handle(pattern, h) }

func (a *App) Group(prefix string, mws ...Middleware) *Group {
	return &Group{app: a, prefix: prefix, mws: mws}
}

func (a *App) OnShutdown(fn func(context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onStop = append(a.onStop, fn)
}

func (a *App) Handler() http.Handler {
	a.buildOnce.Do(func() {
		a.mux.Handle("GET /healthp", a.opts.Prober.HealthPHandler())
		a.mux.Handle("GET /healthz", monitoring.HealthZHandler())
		a.mux.Handle("GET /readyz", a.opts.Prober.ReadinessHandler())
		a.mux.Handle("GET /metrics", a.opts.Metrics.Handler())
		if a.opts.StaticDir != "" {
			prefix := a.opts.StaticPrefix
			if prefix == "" {
				prefix = "/static/"
			}
			a.mux.Handle("GET "+prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(a.opts.StaticDir))))
		}
		notFound := HandlerFunc(func(http.ResponseWriter, *http.Request) error { return E(CodeRouteNotFound) })
		var fallback http.Handler = notFound
		if a.opts.RateLimit != nil {
			rl := *a.opts.RateLimit
			if rl.KeyFunc == nil {
				trusted := a.opts.TrustedProxies
				rl.KeyFunc = func(r *http.Request) string { return ClientIP(r, trusted) }
			}
			fallback = RateLimit(rl)(notFound)
		}
		a.mux.Handle("/", fallback)

		mws := []Middleware{
			monitoring.TrackRoute,
			RequestIDs(a.opts.Logger),
			Recover(),
			AccessLog(nil),
			a.opts.Metrics.Middleware(nil),
			SecurityHeaders(a.opts.Security),
		}
		if a.opts.ErrorReporter != nil {
			mws = append(mws, WithErrorReporter(a.opts.ErrorReporter))
		}
		if a.opts.CORS != nil {
			mws = append(mws, CORS(*a.opts.CORS))
		}
		mws = append(mws, BodyLimit(a.opts.MaxBodyBytes), Timeout(a.opts.RequestTimeout))
		mws = append(mws, a.opts.Middleware...)
		a.handler = Chain(monitoring.RecordRoute(a.mux), mws...)
	})
	return a.handler
}

func (a *App) Server(addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadTimeout:       a.opts.ReadTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      a.opts.WriteTimeout,
		IdleTimeout:       a.opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(a.startDrain)
	return srv
}

func (a *App) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return a.Serve(ctx, ln)
}

func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := a.Server(ln.Addr().String())
	srv.BaseContext = func(net.Listener) context.Context { return context.WithoutCancel(ctx) }

	a.opts.Prober.Start(context.WithoutCancel(ctx))
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	a.log.Info("server started",
		slog.String("name", a.opts.Name),
		slog.String("addr", ln.Addr().String()),
		slog.String("environment", a.opts.Environment))

	select {
	case err := <-errCh:
		a.opts.Prober.Stop()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	a.log.Info("shutting down", slog.Duration("timeout", a.opts.ShutdownTimeout))
	sctx, cancel := context.WithTimeout(context.Background(), a.opts.ShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	a.opts.Prober.Stop()

	a.mu.Lock()
	hooks := append([]func(context.Context) error(nil), a.onStop...)
	a.mu.Unlock()
	for i := len(hooks) - 1; i >= 0; i-- {
		err = errors.Join(err, hooks[i](sctx))
	}
	if err != nil {
		a.log.Error("shutdown finished with errors", slog.Any("error", err))
		return err
	}
	a.log.Info("shutdown complete")
	return nil
}

type Group struct {
	app    *App
	prefix string
	mws    []Middleware
}

func (g *Group) Group(prefix string, mws ...Middleware) *Group {
	return &Group{app: g.app, prefix: g.prefix + prefix, mws: append(append([]Middleware{}, g.mws...), mws...)}
}

func (g *Group) Use(mws ...Middleware) { g.mws = append(g.mws, mws...) }

func (g *Group) Handle(method, path string, h http.Handler, mws ...Middleware) {
	all := append(append([]Middleware{}, g.mws...), mws...)
	pattern := g.prefix + path
	if method != "" {
		pattern = method + " " + pattern
	}
	g.app.mux.Handle(pattern, Chain(h, all...))
}

func (g *Group) Get(path string, h http.Handler, mws ...Middleware) {
	g.Handle(http.MethodGet, path, h, mws...)
}

func (g *Group) Post(path string, h http.Handler, mws ...Middleware) {
	g.Handle(http.MethodPost, path, h, mws...)
}

func (g *Group) Put(path string, h http.Handler, mws ...Middleware) {
	g.Handle(http.MethodPut, path, h, mws...)
}

func (g *Group) Patch(path string, h http.Handler, mws ...Middleware) {
	g.Handle(http.MethodPatch, path, h, mws...)
}

func (g *Group) Delete(path string, h http.Handler, mws ...Middleware) {
	g.Handle(http.MethodDelete, path, h, mws...)
}
