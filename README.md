# smsd (this repo: `sms`)

smsd turns an Android phone you already own into a small, careful SMS API. Send one message or a bulk list over HTTP; smsd queues them and hands them one at a time to [SMSGate](https://github.com/capcom6/android-sms-gateway) on the phone, which sends them from **one chosen SIM**. Sending is paced well under PTA and carrier limits, with daily caps, warm-up and a circuit breaker. A job interrupted at any point (a restart, a dead phone, a cancel) resumes without sending anything twice.

It is meant for small volumes, such as OTPs, reminders and group notices, where a paid SMS gateway is overkill. Respect your local SMS and consent rules when you use it.

**Stack:** Go, SQLite, OpenAPI 3.1 · **License:** MIT

## Features

- Three ways to send: one message, the same text to many numbers, or a different text per number
- Every call returns `202` with a job right away; watch progress, cancel or resume from the API or the built-in sender page
- Six gates before every message: sending hours, daily cap, a rolling 15-minute window, random spacing (6–9 s by default), backpressure on the previous message, and a failure circuit breaker
- Warm-up: the first messages of a bulk job go out slower and must resolve before the rest are sent
- No double sends: a send ledger in SQLite makes resume safe after any interruption
- Phone number normalization and SMS segment counting
- Swagger UI and an OpenAPI 3.1 spec served by the binary
- Single static binary, no cgo

## Endpoints

| URL | What |
|---|---|
| `/` | Sender page: compose, send, watch jobs, cancel or resume |
| `/docs` | Swagger UI; "Try it out" works once you're signed in |
| `/openapi.json` | OpenAPI 3.1 spec |
| `/api/...` | The API |
| `/healthz` | `ok`, no login |

Everything except `/healthz` needs HTTP Basic auth, set with `SMS_AUTH_USER` and `SMS_AUTH_PASSWORD`. A browser asks once and remembers it for the session; with curl, use `-u user:password`.

## Quick start

Needs Go 1.26 and a phone running the SMSGate app.

```bash
go build -o bin/smsd ./cmd/smsd
SMS_AUTH_USER=sms SMS_AUTH_PASSWORD=change-me-please SMS_GATEWAY_URL=http://<phone-ip>:8080 \
  SMS_SIM=1 bin/smsd                            # http://127.0.0.1:8097, data in ./data
```

Then send through your instance:

```bash
BASE=http://127.0.0.1:8097
LOGIN=sms:change-me-please   # SMS_AUTH_USER:SMS_AUTH_PASSWORD

# One message
curl -u "$LOGIN" -X POST $BASE/api/send \
  -d '{"to": "03001234567", "body": "Salam, the meeting moved to 5pm."}'

# Same text to many numbers
curl -u "$LOGIN" -X POST $BASE/api/bulk \
  -d '{"to": ["03001234567", "03007654321"], "body": "Salam, the meeting moved to 5pm."}'

# A different text per number
curl -u "$LOGIN" -X POST $BASE/api/bulk \
  -d '{"messages": [{"to": "03001234567", "body": "Salam Ali"}, {"to": "03007654321", "body": "Salam Sara"}]}'

# Progress
curl -u "$LOGIN" $BASE/api/jobs/<id>
```

All settings (pacing, caps, hours, warm-up, breaker thresholds) are `SMS_*` environment variables with defaults and allowed ranges in `internal/config/config.go`. `deploy/` has a systemd unit, an nginx site template and deploy scripts for running your own instance (see below).

## Deploy your own

You need a Linux server with nginx, certbot and Python 3, a domain pointing at it, and SSH access with sudo.

```bash
SMSD_SERVER=ubuntu@your-server SMSD_DOMAIN=sms.example.com deploy/deploy.sh
```

The script runs the tests, cross-compiles the binary (`SMSD_GOARCH`, default `arm64`), uploads it and activates it as a new release. On the first deploy it creates `/etc/smsd/smsd.env` with a random login; fill in your SMSGate credentials there. Each release is health-checked and rolled back automatically if it does not come up. Details in [docs/operations.md](docs/operations.md).

## Tech stack

Go standard library HTTP server, SQLite via `modernc.org/sqlite` (pure Go), embedded HTML sender page and Swagger UI, SMSGate for Android as the phone-side gateway.

## How it works

```
cmd/smsd/              entry point: reads env, opens the database, starts the worker and HTTP server
internal/api/          HTTP handlers (server.go), the job worker (worker.go)
internal/api/web/      sender page, Swagger UI page and openapi.json, embedded in the binary
internal/engine/       pacing, caps, warm-up, circuit breaker, resume; the only path to the phone
internal/gateway/      SMSGate HTTP client and an in-memory fake for tests
internal/store/        SQLite: jobs, messages, send ledger
internal/config/       settings, defaults and allowed ranges, read from SMS_* env vars
internal/msg/          phone number normalization and SMS segment counting
deploy/                deploy scripts, systemd unit, nginx site
docs/                  API, architecture and operations docs
```

- [docs/api.md](docs/api.md): every endpoint, request and response, validation, job and message states, errors.
- [docs/architecture.md](docs/architecture.md): how a request becomes an SMS, including the engine's six gates, the database and the no-double-send rules.
- [docs/operations.md](docs/operations.md): phone setup, configuration, deploys and rollback, logs, and what to do when a job pauses or stops.

## Testing

```bash
go test ./...   # engine, API, config, gateway, msg; uses a fake gateway, no phone needed
```

## License

[MIT](LICENSE)

Author: Huzaifa Nur — https://www.huzaifanur.com
