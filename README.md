# rc_vistart

> **TODO — project description.** This section is a placeholder and will be
> replaced with the official introduction once it is provided.

An internal delivery service. Business systems submit orders to it; it notifies
the downstream endpoints those systems registered, and keeps trying until each
notification is acknowledged.

- **Accepts** order submissions over a small JSON HTTP API.
- **Delivers** each step of an order to the downstream service that registered
  for it, retrying with backoff and escalating whatever never lands.
- **Tracks** every attempt, so "did we send it?" and "did we send it twice?"
  are both answerable after the fact.

No third-party dependencies — only the Go standard library.

## The guarantee, stated honestly

Exactly-once delivery is not achievable over HTTP: a receiver that crashes after
acting but before answering leaves the sender unable to tell "done" from "never
arrived". `rc_vistart` therefore guarantees **at-least-once delivery per task**,
paired with two things that make that safe:

1. **A task key that survives retries.** Every step of an order has the stable
   identity `<order id>/<step name>`. It does not change across retries, so a
   receiver can tell a retry from a new task and apply the effect once.
2. **An audit trail.** Every attempt is recorded, so a missing or repeated
   notification is visible rather than silent.

Losing a notification is treated as the worse failure, because a duplicate
notice is recoverable downstream and a dropped one is not.

> **Both sides are responsible.** `rc_vistart` will retry until acknowledged, and
> will retry a step whose *result* never arrives. The receiver must deduplicate
> on the task key. `cmd/mock_service` shows the pattern.

## How an order works

An order is a fixed graph of steps, created once and never mutated. Each step is
one task with its own identity.

```
phase "prepare"                     phase "produce"
  verify.identity  (callback) ─┐
  verify.stock     (callback)  ├─ all must succeed ─┐
  verify.payment   (callback) ─┘                     │
                                                      ▼
                                        produce.delivery (notify)
                                                  │
                                                  ▼
                                        stock.outbound   (notify)
```

A step is one of two kinds:

| Kind        | Finished when                                                     |
| ----------- | ----------------------------------------------------------------- |
| `notify`    | The receiver answers with an expected status code.                |
| `callback`  | The receiver *also* posts a result to the URL it was given.        |

`callback` exists because an HTTP `200` only means the request was understood.
Stock pre-deduction needs to know whether the stock was actually reserved, so
the step waits for an answer — and if that answer never comes, the step is
retried, then escalated.

Dependencies must stay inside a phase. Moving to the next phase is what gates
it, which is why `produce.delivery` declares no dependency on the checks: it
cannot run until the prepare phase has fully succeeded.

## Built-in presets

| Preset           | Steps                                                     |
| ---------------- | --------------------------------------------------------- |
| `order.standard` | Identity, stock pre-deduction, payment → delivery, outbound |
| `ad.registration`| Notify the ad system that a registration succeeded         |
| `crm.contact`    | Move a CRM contact to a new status                         |

`GET /api/v1/presets` returns them as JSON. A caller may pass a preset, supply
its own step graph, or take the standard order by default.

## Requirements

- Go 1.27 or newer.

## Quick start

```bash
go run ./cmd/rc_vistart          # listens on :8080
curl http://localhost:8080/healthz
```

## Try it with the mock downstream

`cmd/mock_service` stands in for any downstream service: it registers itself,
receives notifications, reports results, and deduplicates on the task key. Its
flags make the reliability paths easy to demonstrate.

```bash
go run ./cmd/rc_vistart &

# A downstream that fails its first two attempts, then succeeds.
go run ./cmd/mock_service \
  -addr 127.0.0.1:9001 \
  -service inventory -action stock.outbound \
  -fail-first 2 -max-attempts 5
```

Submit an order and watch it complete:

```bash
curl -X POST http://localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{"external_ref":"BIZ-1","subject":"user-42",
       "items":[{"sku":"SKU-1","quantity":2}]}'
```

Then ask the mock what it received:

```bash
curl http://127.0.0.1:9001/state
```

```json
{
  "tasks": [
    {
      "task_key": "ord_.../stock.outbound",
      "attempts": 3,
      "applied": 1
    }
  ],
  "effects": 1
}
```

Three attempts were made, and the business effect was applied **once**. That is
the anti-duplication guarantee in one object.

Useful mock flags:

| Flag             | Demonstrates                                        |
| ---------------- | --------------------------------------------------- |
| `-fail-first N`  | Retry, then success                                 |
| `-fail-forever`  | Escalation after the attempt budget is spent        |
| `-silent`        | Acknowledged but no result → result-timeout retry   |

## Registering a downstream endpoint

Each destination registers itself. Re-registering the same topic replaces the
previous registration, keeping the id, so a service can rotate its URL without
creating a second destination for one task.

```bash
curl -X POST http://localhost:8080/api/v1/endpoints \
  -H 'Content-Type: application/json' \
  -d '{
        "service": "inventory",
        "action":  "stock.prehold",
        "method":  "POST",
        "url":     "https://inventory.internal/hooks/prehold",
        "headers": [{"name": "X-Api-Key", "value": "{{ .secret }}"}],
        "body":    "{\"order\":\"{{ .order.id }}\",\"items\":{{ json .items }}}",
        "expect_status": [200, 202],
        "timeout": "3s",
        "result_timeout": "30s",
        "max_attempts": 5,
        "backoff_base": "500ms",
        "backoff_max": "30s"
      }'
```

| Field            | Meaning                                                             |
| ---------------- | ------------------------------------------------------------------- |
| `service`, `action` | The topic is `<service>.<action>`, e.g. `inventory.stock.prehold`. |
| `method`         | `POST`, `PUT` or `PATCH`.                                           |
| `url`            | Templated target URL.                                               |
| `headers`, `body`| Templated request headers and body.                                 |
| `expect_status`  | Status codes that count as an acknowledgement.                      |
| `timeout`        | Deadline for one attempt, including the wait for the response.      |
| `result_timeout` | How long a `callback` step waits for its result.                    |
| `max_attempts`   | Attempts before the task is escalated.                              |
| `backoff_*`      | Exponential retry delay bounds.                                     |
| `secret`         | Shared key exposed to templates as `.secret`.                       |

Registration validates everything up front: the templates are parsed, rendered
against sample data, and checked for references that could never resolve. A
registration that cannot work is rejected with `400` rather than failing on the
third retry of a live order.

### Template context

| Reference           | Value                                                   |
| ------------------- | ------------------------------------------------------- |
| `.order.id`         | Order id.                                               |
| `.order.external_ref` | The caller's own reference.                           |
| `.order.subject`    | Subject of the order.                                   |
| `.order.phase`      | `prepare` or `produce`.                                 |
| `.step.name`        | Step name.                                              |
| `.step.attempt`     | Attempt number, starting at 1.                          |
| `.step.topic`       | Topic being delivered to.                               |
| `.items`            | List of `{sku, name, quantity, price}`.                 |
| `.payload`          | The caller-supplied payload.                            |
| `.idempotency_key`  | The stable task key.                                    |
| `.delivery_id`      | This attempt's id. Differs per attempt, for tracing.    |
| `.result_url`       | Where to post a result for a `callback` step.           |
| `.secret`           | The registration's shared secret.                       |
| `.timestamp`, `.unix` | Send time, RFC 3339 and Unix seconds.                  |
| `.body`             | The rendered body. **Header templates only.**          |
| `.signature`        | `HMAC-SHA256(secret, "<unix>.<body>")`. **Headers only.** |

Helpers: `json`, `urlquery`, `hmacSHA256`, `default`, `upper`, `lower`, `trim`.

The URL and the body render first, then the headers — so a header can sign the
body, which is what reproducing a vendor's own signing scheme requires. For the
same reason `.signature` and `.body` are rejected inside the URL and body: the
signature covers the body, so it cannot appear within it.

An absent key in `.payload` fails the delivery rather than sending a blank field
to a vendor. Use `{{ default "x" (index .payload "maybe") }}` for an optional
value.

## HTTP API

| Method   | Path                                          | Purpose                             |
| -------- | --------------------------------------------- | ----------------------------------- |
| `GET`    | `/healthz`                                    | Liveness probe.                     |
| `POST`   | `/api/v1/endpoints`                           | Register a destination.             |
| `GET`    | `/api/v1/endpoints`                           | List registrations.                 |
| `DELETE` | `/api/v1/endpoints/{topic}`                   | Remove a registration.              |
| `GET`    | `/api/v1/presets`                             | Built-in step graphs.               |
| `POST`   | `/api/v1/orders`                               | Submit an order.                    |
| `GET`    | `/api/v1/orders`                               | List orders, optionally `?state=`.  |
| `GET`    | `/api/v1/orders/{id}`                          | Inspect an order and its ledger.    |
| `POST`   | `/api/v1/orders/{id}/steps/{step}/result`      | Post a step result (receivers).     |
| `GET`    | `/api/v1/stats`                                | Per-endpoint delivery statistics.   |
| `GET`    | `/api/v1/escalations`                          | Tasks needing a human.              |

### Reporting a result

```bash
curl -X POST http://localhost:8080/api/v1/orders/$ORDER/steps/verify.stock/result \
  -H 'Content-Type: application/json' \
  -d '{"delivery_id":"'$DELIVERY'","success":true,"data":{"reserved":2}}'
```

`delivery_id` must echo the `X-Rc-Vistart-Delivery` header of the delivery being
answered, which is how a late or forged answer is rejected. Reposting an answer
is harmless: a step that already finished ignores it rather than transitioning
twice.

`success: false` is a business rejection, not a transport failure, so it escalates
immediately instead of burning the retry budget.

### Errors

```json
{
  "error": "invalid_registration",
  "message": "notify: invalid endpoint: body: .signature covers the body itself, so it is only available to header templates"
}
```

`error` is a stable code (`invalid_request`, `invalid_registration`,
`invalid_order`, `not_found`, `conflict`, `internal_error`); `message` is for
humans.

## Retries and escalation

| Failure                                | Retried?                                    |
| -------------------------------------- | ------------------------------------------- |
| Network error, timeout, `5xx`, `429`   | Yes, with exponential backoff.              |
| No result within `result_timeout`      | Yes — this is "we never heard back".        |
| `4xx`, redirect, unrenderable template | No. Retrying cannot fix a wrong request.    |
| No endpoint registered for the topic   | Yes, then escalated. It may register later. |

After the budget is spent the step becomes `escalated`, the order fails, and the
task is listed in `/api/v1/escalations`. Dependent steps are `skipped` rather
than left pending, so nothing waits forever.

Every attempt appears in the step's ledger, including its status code, latency
and error.

## Monitoring

```bash
curl http://localhost:8080/api/v1/stats
```

Reports `attempts`, `delivered`, `failed`, `escalated` and mean latency per
topic. Escalation is additionally logged at `ERROR` with the task key, which is
the hook a real deployment would page from.

## Configuration

| Variable                                | Default                 | Meaning                            |
| --------------------------------------- | ----------------------- | ---------------------------------- |
| `RC_VISTART_ADDR`                       | `:8080`                 | Listen address.                    |
| `RC_VISTART_PUBLIC_URL`                 | `http://localhost:8080` | Base URL receivers post results to.|
| `RC_VISTART_WORKERS`                    | `8`                     | Concurrent delivery attempts.      |
| `RC_VISTART_POLL_INTERVAL`              | `50ms`                  | Idle worker re-check interval.     |
| `RC_VISTART_READ_TIMEOUT`               | `5s`                    | Read deadline per request.         |
| `RC_VISTART_WRITE_TIMEOUT`              | `10s`                   | Write deadline per response.       |
| `RC_VISTART_IDLE_TIMEOUT`               | `60s`                   | Idle keep-alive timeout.           |
| `RC_VISTART_SHUTDOWN_TIMEOUT`           | `15s`                   | Grace period on SIGINT/SIGTERM.    |
| `RC_VISTART_MAX_BODY_BYTES`             | `1048576`               | Maximum request body.              |
| `RC_VISTART_MAX_CALLBACK_RESPONSE_BYTES`| `65536`                 | Response bytes read per delivery.  |
| `RC_VISTART_LOG_LEVEL`                  | `info`                  | `debug`, `info`, `warn`, `error`.  |
| `RC_VISTART_LOG_FORMAT`                 | `json`                  | `json` or `text`.                  |

> **`RC_VISTART_PUBLIC_URL` matters.** Receivers are told where to post results
> through it. If it is unreachable from their network, every `callback` step
> will time out and be retried.

`rc_vistart` shuts down gracefully on `SIGINT`/`SIGTERM`, letting in-flight
deliveries finish within the grace period.

## Prototype limitations

Stated plainly, because the design is otherwise easy to over-read:

- **State is in memory.** Endpoint registrations and orders are lost on restart.
  A production version needs durable storage for both; the `notify.Registry`
  interface and the order store are where that would plug in.
- **The step result endpoint is unauthenticated** beyond matching the delivery
  id. A real deployment needs per-service credentials or mTLS.
- **Escalation is a log line and an API view**, not a page.
- **The queue is a sorted slice**, fine for thousands of pending tasks, not
  millions.
- **`rc_vistart` never cancels work** it has already queued during shutdown
  beyond the grace period.

## Development

There is no remote CI for this project — verification is local.

```bash
make check   # gofmt check, go vet, tests
make test    # go test ./...
make cover   # coverage.out + coverage.html
make build   # bin/rc_vistart and bin/mock_service
make help    # list every target
```

Without `make` (or on Windows, where `make` may not be installed):

```bash
gofmt -s -l . && go vet ./... && go test ./...
go build -o bin/rc_vistart.exe ./cmd/rc_vistart
go build -o bin/mock_service.exe ./cmd/mock_service
```

> **Note on `-race`:** `make test-race` runs `go test -race ./...`, which needs a
> working race-enabled toolchain (CGO plus a supported C compiler). On the
> current Windows/mingw setup it fails to start with an entry-point error before
> any test runs, so treat it as optional until the toolchain is fixed.

## Project layout

```
cmd/rc_vistart/     Service entry point, wiring and graceful shutdown
cmd/mock_service/   Simulated downstream, for demos and manual testing
internal/notify/    Endpoint registry, templates, HTTP invoker, queue, stats
internal/order/     Order model, presets and the orchestration engine
internal/httpapi/   Routing and request/response handling
internal/config/    Environment-based configuration
internal/version/   Build metadata injected via -ldflags
```

The delivery engine does not know about orders and the engine does not know
about HTTP; they meet only at the engine's delivery step.

## License

[MIT](LICENSE) © 2026 vistart