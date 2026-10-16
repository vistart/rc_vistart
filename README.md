# rc_vistart

> **TODO — project description.** This section is a placeholder and will be
> replaced with the official introduction once it is provided.

An HTTP service that receives requests and forwards them to callbacks that other
services registered.

- **Receives requests** over a small JSON HTTP API.
- **Calls registered callbacks**: other services register an HTTP endpoint for a
  topic, and `rc_vistart` invokes every registered endpoint whenever an event for
  that topic arrives.

## Status

Under development. The public API may still change.

## Table of contents

- [Requirements](#requirements)
- [Quick start](#quick-start)
- [How it works](#how-it-works)
- [HTTP API](#http-api)
- [Callback invocation](#callback-invocation)
- [Configuration](#configuration)
- [Development](#development)
- [Project layout](#project-layout)
- [License](#license)

## Requirements

- Go 1.27 or newer.

The project has **no third-party dependencies** — only the standard library.

## Quick start

```bash
git clone https://github.com/vistart/rc_vistart.git
cd rc_vistart
go build -o bin/rc_vistart ./cmd/rc_vistart
./bin/rc_vistart          # listens on :8080
```

Or run it straight from source:

```bash
go run ./cmd/rc_vistart
```

Check that it is up:

```bash
curl http://localhost:8080/healthz
```

```json
{
  "status": "ok"
}
```

## How it works

Two independent flows share one process.

**1. Registration** — a service tells `rc_vistart` which endpoint to call for a
given topic:

```
POST /api/v1/callbacks  {"topic":"order.created","url":"https://billing/hooks"}
```

**2. Dispatch** — an event arrives and is forwarded to every callback that is
registered for its topic:

```
POST /api/v1/events/order.created  {"id":"order-1"}
  -> POST https://billing/hooks    {"id":"order-1"}
```

Registrations are held **in memory**: they live for as long as the process does
and are lost on restart. Re-register on startup, or use a service that keeps
this registry authoritative.

A failing callback never fails the publisher's request. The dispatch endpoint
returns `202 Accepted` with a per-callback delivery report in the body, so one
broken subscriber cannot take down the others.

## HTTP API

| Method   | Path                        | Purpose                                   |
| -------- | --------------------------- | ----------------------------------------- |
| `GET`    | `/healthz`                  | Liveness probe.                           |
| `POST`   | `/api/v1/callbacks`         | Register a callback endpoint.             |
| `GET`    | `/api/v1/callbacks`         | List registrations, optionally by `?topic=`. |
| `DELETE` | `/api/v1/callbacks/{id}`    | Remove a registration.                    |
| `POST`   | `/api/v1/events/{topic}`    | Publish an event to that topic's callbacks. |

### Register a callback

```bash
curl -X POST http://localhost:8080/api/v1/callbacks \
  -H 'Content-Type: application/json' \
  -d '{
        "topic":   "order.created",
        "url":     "https://billing.example.com/hooks",
        "secret":  "shared-secret",
        "timeout": "3s"
      }'
```

```json
{
  "id": "2cf40b36a1caa8b78988d5c217fb133b",
  "topic": "order.created",
  "url": "https://billing.example.com/hooks",
  "timeout": "3s",
  "created_at": "2026-10-16T04:33:13.573973Z"
}
```

| Field     | Type   | Required | Notes                                                        |
| --------- | ------ | -------- | ------------------------------------------------------------ |
| `topic`   | string | yes      | Event name. No whitespace and no `/`.                          |
| `url`     | string | yes      | Absolute `http`/`https` endpoint.                             |
| `secret`  | string | no       | Enables HMAC-SHA256 signing of invocations.                   |
| `timeout` | string | no       | Go duration such as `3s`. Defaults to the service setting.    |

`secret` is **write-only**: it is accepted on registration but never returned
by any endpoint, so a stored secret never leaks through the list API.

### List callbacks

```bash
curl http://localhost:8080/api/v1/callbacks
curl 'http://localhost:8080/api/v1/callbacks?topic=order.created'
```

### Remove a callback

```bash
curl -X DELETE http://localhost:8080/api/v1/callbacks/2cf40b36a1caa8b78988d5c217fb133b
```

Returns `204 No Content`, or `404` if the id is unknown.

### Publish an event

```bash
curl -X POST http://localhost:8080/api/v1/events/order.created \
  -H 'Content-Type: application/json' \
  -d '{"id":"order-1","total":4200}'
```

```json
{
  "topic": "order.created",
  "delivery_id": "e3defd339228903d6d28a7216d968c25",
  "delivered": 1,
  "failed": 0,
  "skipped": 0,
  "deliveries": [
    {
      "callback_id": "2cf40b36a1caa8b78988d5c217fb133b",
      "url": "https://billing.example.com/hooks",
      "status": "delivered",
      "duration": 529400
    }
  ]
}
```

The body must be non-empty, valid JSON, and within the size limit.

Every callback subscribed to the topic is invoked concurrently, up to
`RC_VISTART_MAX_PARALLEL_CALLBACKS` at a time. Each entry in `deliveries` carries
one of three statuses:

| `status`     | Meaning                                                                  |
| ------------ | ------------------------------------------------------------------------ |
| `delivered`  | The callback answered `2xx`.                                             |
| `failed`     | The callback errored, timed out, or answered non-`2xx`.                  |
| `skipped`    | Not attempted, because the request ended (client disconnect or shutdown). |

`duration` is in nanoseconds, matching Go's `time.Duration`.

### Errors

Every non-2xx response uses the same shape:

```json
{
  "error": "invalid_request",
  "message": "callback: invalid registration: topic must not be empty"
}
```

`error` is a stable machine-readable code (`invalid_request`, `not_found`,
`internal_error`); `message` is for humans.

## Callback invocation

Each subscriber receives an `HTTP POST` with the event body verbatim and these
headers:

| Header                      | Value                                        |
| --------------------------- | -------------------------------------------- |
| `Content-Type`              | `application/json`                            |
| `X-Rc-Vistart-Event`        | The topic.                                    |
| `X-Rc-Vistart-Delivery`     | Shared id of this dispatch round.             |
| `X-Rc-Vistart-Timestamp`    | Send time, RFC 3333.                          |
| `X-Rc-Vistart-Signature`    | `sha256=<hex>` — only when a secret is set.   |

Use `X-Rc-Vistart-Delivery` to deduplicate, and the signature to authenticate.
Any `2xx` counts as delivered; anything else is recorded as a failure.

### Verifying the signature

The signed message is `<unix-timestamp>.<body>`:

```go
func verify(secret, timestamp string, body []byte, signature string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(signature, prefix)))
}
```

Reject timestamps outside a small window to avoid replay attacks. The identical
implementation is available as `callback.VerifySignature` for receivers that
already depend on this module.

## Configuration

All configuration is environment based.

| Variable                                | Default   | Meaning                                   |
| --------------------------------------- | --------- | ----------------------------------------- |
| `RC_VISTART_ADDR`                       | `:8080`   | Listen address.                           |
| `RC_VISTART_READ_TIMEOUT`               | `5s`      | Limit for reading a whole request.        |
| `RC_VISTART_WRITE_TIMEOUT`              | `10s`     | Limit for writing a response.             |
| `RC_VISTART_IDLE_TIMEOUT`               | `60s`     | Idle keep-alive timeout.                  |
| `RC_VISTART_SHUTDOWN_TIMEOUT`           | `15s`     | Grace period on SIGINT/SIGTERM.           |
| `RC_VISTART_MAX_BODY_BYTES`             | `1048576` | Maximum accepted request body.            |
| `RC_VISTART_CALLBACK_TIMEOUT`           | `5s`      | Timeout for callbacks without their own.  |
| `RC_VISTART_MAX_CALLBACK_RESPONSE_BYTES`| `65536`   | Response bytes read from a callback.      |
| `RC_VISTART_MAX_PARALLEL_CALLBACKS`     | `8`       | Concurrent callbacks per dispatch.        |
| `RC_VISTART_LOG_LEVEL`                  | `info`    | `debug`, `info`, `warn`, `error`.         |
| `RC_VISTART_LOG_FORMAT`                 | `json`    | `json` or `text`.                         |

`rc_vistart` shuts down gracefully on `SIGINT`/`SIGTERM`: it stops accepting
connections and lets in-flight dispatches finish within the grace period.

## Development

There is no remote CI for this project — verification is local.

With `make`:

```bash
make check   # gofmt check, go vet, tests
make test    # go test ./...
make cover   # coverage.out + coverage.html
make build   # bin/rc_vistart
make help    # list every target
```

Without `make` (or on Windows, where `make` may not be installed):

```bash
gofmt -s -l . && go vet ./... && go test ./...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
go build -o bin/rc_vistart ./cmd/rc_vistart
```

> **Note on `-race`:** `make test-race` runs `go test -race ./...`, which needs a
> working race-enabled toolchain (CGO plus a supported C compiler). On the
> current Windows/mingw setup it fails to start with an entry-point error before
> any test runs, so treat it as optional until the toolchain is fixed.

Dependencies are pinned in `go.mod`; run `go mod tidy` after changing imports.

## Project layout

```
cmd/rc_vistart/       Binary entry point, wiring and graceful shutdown
internal/callback/    Registry, HTTP invoker, dispatcher and signing
internal/config/      Environment-based configuration
internal/httpapi/     HTTP routing and request/response handling
internal/version/     Build metadata injected via -ldflags
```

The callback registry is behind the `callback.Registry` interface, so an
alternative backing store can be dropped in without touching the HTTP layer.

## License

[MIT](LICENSE) © 2026 vistart