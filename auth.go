package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ahmedbaig/go-backend-logging"
	"github.com/ahmedbaig/go-backend-types"
)

func IDs(ctx context.Context) (string, string) { return RequestID(ctx), CorrelationID(ctx) }

func RequireUser() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, err := types.ParseUser(r.Header.Get(types.HeaderUser))
			if err != nil {
				e := E(CodeUnauthenticated)
				if r.Header.Get(types.HeaderUser) != "" {
					e = e.WithMessage("The session header is invalid")
				}
				WriteError(w, r, e.Wrap(err))
				return
			}
			ctx := types.WithUser(r.Context(), u)
			ctx = logging.WithAttrs(ctx, slog.Int64("userId", u.ID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func RequireRole(roles ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := types.UserFrom(r.Context())
			if !ok {
				WriteError(w, r, E(CodeUnauthenticated))
				return
			}
			if !u.HasAny(roles...) {
				WriteError(w, r, E(CodeForbidden))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func RequireFeature(feature string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := types.UserFrom(r.Context())
			if !ok || !u.Can(feature) {
				WriteError(w, r, E(CodeForbidden).WithMessage("This feature is not enabled for your account"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func User(ctx context.Context) (types.User, error) {
	u, ok := types.UserFrom(ctx)
	if !ok {
		return types.User{}, E(CodeUnauthenticated)
	}
	return u, nil
}
