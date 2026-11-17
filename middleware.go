package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ahmedbaig/go-backend-logging"
	"github.com/ahmedbaig/go-backend-monitoring"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,128}$`)

func newID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func incomingID(r *http.Request, header string) string {
	if v := r.Header.Get(header); safeID.MatchString(v) {
		return v
	}
	return newID()
}

func RequestIDs(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := incomingID(r, HeaderRequestID)
			cid := incomingID(r, HeaderCorrelationID)
			if r.Header.Get(HeaderCorrelationID) == "" {
				cid = rid
			}
			w.Header().Set(HeaderRequestID, rid)
			w.Header().Set(HeaderCorrelationID, cid)
			ctx := WithIDs(logging.WithScope(r.Context()), rid, cid)
			ctx = logging.WithAttrs(ctx, slog.String("requestId", rid), slog.String("correlationId", cid))
			if log != nil {
				ctx = WithLogger(ctx, log)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func ClientInfo(trusted []netip.Prefix) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := logging.WithAttrs(r.Context(), slog.String("ip", ClientIP(r, trusted)), slog.String("userAgent", r.UserAgent()))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func AccessLog(skip func(*http.Request) bool) Middleware {
	if skip == nil {
		skip = SkipHealth
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			lvl := slog.LevelInfo
			if status >= 400 {
				lvl = slog.LevelError
				if status < 500 {
					lvl = slog.LevelWarn
				}
			}
			LoggerFrom(r.Context()).Log(r.Context(), lvl,
				fmt.Sprintf("%s %s %d %dms", r.Method, r.URL.RequestURI(), status, time.Since(start).Milliseconds()),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("durationMs", time.Since(start).Milliseconds()),
				slog.String("route", monitoring.RoutePattern(r)),
			)
		})
	}
}

func SkipHealth(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/health") || r.URL.Path == "/metrics"
}

func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				LoggerFrom(r.Context()).ErrorContext(r.Context(), "panic in handler",
					slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
				WriteError(w, r, E(CodeInternal).Wrap(fmt.Errorf("panic: %v", v)))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type SecurityHeadersOptions struct {
	HSTS                  bool
	ContentSecurityPolicy string
}

func SecurityHeaders(o SecurityHeadersOptions) Middleware {
	csp := o.ContentSecurityPolicy
	if csp == "" {
		csp = "default-src 'none'; frame-ancestors 'none'"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy", csp)
			if o.HSTS {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

type CORSOptions struct {
	AllowedOrigins   []string
	AllowedMethods   []string
	AllowedHeaders   []string
	ExposedHeaders   []string
	AllowCredentials bool
	MaxAge           time.Duration
}

func CORS(o CORSOptions) Middleware {
	allowAll := false
	origins := map[string]bool{}
	for _, v := range o.AllowedOrigins {
		if v == "*" {
			allowAll = true
		}
		origins[strings.ToLower(v)] = true
	}
	methods := strings.Join(orDefault(o.AllowedMethods, []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}), ", ")
	headers := strings.Join(orDefault(o.AllowedHeaders, []string{"Content-Type", "Authorization", HeaderCorrelationID, HeaderRequestID}), ", ")
	exposed := strings.Join(orDefault(o.ExposedHeaders, []string{HeaderCorrelationID, HeaderRequestID}), ", ")
	maxAge := strconv.Itoa(int(orDuration(o.MaxAge, 10*time.Minute).Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			if !allowAll && !origins[strings.ToLower(origin)] {
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if allowAll && !o.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if o.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			h.Set("Access-Control-Expose-Headers", exposed)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", methods)
				h.Set("Access-Control-Allow-Headers", headers)
				h.Set("Access-Control-Max-Age", maxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func orDefault(v, d []string) []string {
	if len(v) == 0 {
		return d
	}
	return v
}

func orDuration(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
}

func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type RateLimitOptions struct {
	Requests int
	Window   time.Duration
	Burst    int
	KeyFunc  func(*http.Request) string
	Skip     func(*http.Request) bool
}

type visitor struct {
	lim  *rate.Limiter
	seen time.Time
}

func RateLimit(o RateLimitOptions) Middleware {
	if o.Requests <= 0 {
		o.Requests = 100
	}
	if o.Window <= 0 {
		o.Window = 15 * time.Minute
	}
	if o.Burst <= 0 {
		o.Burst = o.Requests
	}
	if o.KeyFunc == nil {
		o.KeyFunc = func(r *http.Request) string { return ClientIP(r, nil) }
	}
	every := rate.Every(o.Window / time.Duration(o.Requests))
	var mu sync.Mutex
	visitors := map[string]*visitor{}
	lastSweep := time.Now()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o.Skip != nil && o.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			key := o.KeyFunc(r)
			now := time.Now()
			mu.Lock()
			if now.Sub(lastSweep) > o.Window {
				for k, v := range visitors {
					if now.Sub(v.seen) > o.Window {
						delete(visitors, k)
					}
				}
				lastSweep = now
			}
			v, ok := visitors[key]
			if !ok {
				v = &visitor{lim: rate.NewLimiter(every, o.Burst)}
				visitors[key] = v
			}
			v.seen = now
			res := v.lim.ReserveN(now, 1)
			delay := res.DelayFrom(now)
			if delay > 0 {
				res.CancelAt(now)
			}
			mu.Unlock()
			if delay > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(delay.Seconds())+1))
				WriteError(w, r, E(CodeRateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if !isTrusted(remote, trusted) {
		return remote.String()
	}
	xff := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, v := range xff {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return remote.String()
}

var LoopbackOnly = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func BodyLimit(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n {
				WriteError(w, r, E(CodeTooLarge))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

func WithErrorReporter(rep ErrorReporter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(withReporter(r.Context(), rep)))
		})
	}
}

var ErrServerClosed = errors.New("server closed")
