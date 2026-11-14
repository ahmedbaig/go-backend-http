# go-backend-http

HTTP server foundation for Go services on the standard library's `net/http` and `ServeMux`. Part of the `go-backend-*` family. Import as `httpx`.

```sh
go get github.com/ahmedbaig/go-backend-http
```

## What `New(...).Handler()` gives you

Every request goes through, in order:

1. **Request and correlation IDs**: `X-Request-ID` and `X-Correlation-ID` are taken from the request (when safe) or generated as UUIDv7, echoed on the response, stored on the context and added to every log line.
2. **Panic recovery**: a panic becomes a 500 error response and an error log with the stack.
3. **Access log**: one line per request, `POST /api/v1/items/7 201 12ms`, with method, path, matched route, status, bytes and duration fields. Health and metrics endpoints are skipped.
4. **Metrics**: from [go-backend-monitoring](https://github.com/ahmedbaig/go-backend-monitoring), labelled by matched route.
5. **Security headers**: `nosniff`, `DENY` framing, no referrer, a strict CSP, optional HSTS.
6. Optional **CORS**, **error reporter** (Sentry or similar), **body size limit**, **request timeout** and your own middleware.

Built-in routes: `GET /healthp` (full probe state), `GET /healthz` (liveness), `GET /readyz` (200 only when stable), `GET /metrics`, optional static files. Anything unmatched gets a 404 error response, optionally rate-limited per client IP to slow down scanners.

## Usage

```go
app := httpx.New(httpx.Options{
	Name:           "my-app",
	Environment:    cfg.Environment,
	Logger:         log.Logger,
	RequestTimeout: 30 * time.Second,
	RateLimit:      &httpx.RateLimitOptions{Requests: 100, Window: 15 * time.Minute},
})

v1 := app.Group("/api/v1", requireUser)
v1.Get("/items/{id}", httpx.Handle(items.Get))
v1.Post("/items", httpx.Handle(items.Create))

app.OnShutdown(func(ctx context.Context) error { return db.Close() })
if err := app.Run(ctx, ":8080"); err != nil {
	log.Error("server stopped", logging.Err(err))
}
```

`Run` serves until SIGINT/SIGTERM or `ctx` is cancelled, then drains in-flight requests, stops the probes and runs the shutdown hooks in reverse order.

## Handlers

`httpx.Handle` turns a typed function into a handler. It binds and validates the request, calls your function and writes JSON.

```go
type CreateItem struct {
	ShopID string `path:"shopId" validate:"required"`
	Name   string `json:"name" validate:"required,min=2"`
	DryRun bool   `query:"dryRun"`
}

func (s *Service) Create(ctx context.Context, in CreateItem) (Item, error) { ... }
```

- `json:"..."` fields come from the body (unknown fields rejected, 10 MB limit), `query:"..."` from the query string (repeat or comma-separate for slices), `path:"..."` from `{wildcards}`.
- `validate:"..."` uses [go-playground/validator](https://github.com/go-playground/validator); failures name the field by its JSON/query/path name.
- Return a value with a `StatusCode() int` method to change the status (e.g. 201).
- For full control, write `httpx.HandlerFunc(func(w, r) error)`; a returned error becomes the error response.

## Errors

Return `httpx.E(code)` (optionally `.WithMessage`, `.WithCustomerActions`, `.WithSupportActions`, `.WithFields`, `.Wrap(cause)`). Every error response has the same shape:

```json
{
  "requestId": "0193...",
  "correlationId": "0193...",
  "errors": [{
    "code": "E400-2",
    "message": "The request failed validation",
    "customerActions": ["Correct the highlighted fields"],
    "fields": [{ "field": "name", "rule": "min", "message": "must be at least 2" }]
  }]
}
```

Built-in codes cover 400 (`E400-1` bad request, `E400-2` validation, `E400-3` invalid JSON), 401, 403, 404 (`E404-1` resource, `E404-2` route), 405, 408, 409, 410, 413, 415, 429, 500, 502, 503 and 504. Add your own with `httpx.Register("E422-1", httpx.Definition{...})`. Any error that isn't an `*httpx.Error` becomes `E500-1`: the client gets a generic message, the real error is logged with the request ID. A request that runs past its deadline becomes `E408`.

## Auth

The gateway authenticates and passes the user as JSON in `X-User` ([go-backend-types](https://github.com/ahmedbaig/go-backend-types) `types.User`).

```go
v1 := app.Group("/api/v1", httpx.RequireUser())
v1.Get("/admin/stats", h, httpx.RequireRole("admin"))
v1.Get("/beta", h, httpx.RequireFeature("beta"))

u, err := httpx.User(ctx)   // 401 error if there is no user
```

`RequireUser` returns `E401-1` when the header is missing or invalid and adds `userId` to every log line for the request.

## Other helpers

| | |
|---|---|
| `RequestID(ctx)`, `CorrelationID(ctx)`, `LoggerFrom(ctx)` | read request-scoped values |
| `IDs(ctx)` | request and correlation IDs, pass to `client.WithIDs` from go-backend-types |
| `NewClient(transport)` | `*http.Client` that forwards `X-Correlation-ID` to downstream services |
| `ClientIP(r, trusted)` | client address, honouring `X-Forwarded-For` only from trusted proxies (loopback by default) |
| `OK`, `Created`, `NoContent`, `JSON`, `WriteError` | response writers |
| `Chain`, `RequestIDs`, `AccessLog`, `Recover`, `SecurityHeaders`, `CORS`, `RateLimit`, `Timeout`, `BodyLimit` | the middleware, usable on their own |

## License

MIT
