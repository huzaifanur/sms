# Architecture

```
client ──HTTPS──▶ nginx ──▶ smsd ──HTTPS──▶ SMSGate cloud relay ◀──push── phone (SMSGate app) ──SMS──▶ recipient
                  (TLS)     127.0.0.1:8097   api.sms-gate.app
```

`smsd` is one Go binary with an SQLite file. The server runs in Oracle Cloud and can't reach
the phone's LAN address, so it talks to SMSGate's cloud relay. The phone keeps an outbound
connection to that relay. Pointing `SMS_GATEWAY_URL` at the phone's Local Server
(`http://<phone-ip>:8080`) works the same way when `smsd` runs on the phone's network. The
client code is identical for both modes.

## Request to SMS

1. **HTTP** (`internal/api/server.go`). A send or bulk request is decoded and each item is
   checked: the number is normalized, the body must be non-empty, and its segment count
   must be within the limit. Repeats are dropped. The valid items become one job with one
   message row each, written in a single transaction with state `queued`. The handler wakes
   the worker and returns `202`. Nothing waits on the phone during the request.
2. **Worker** (`internal/api/worker.go`). One goroutine runs one job at a time. Every send
   therefore passes the same gates in order, and two jobs can never double the rate. It
   picks queued single sends first, then queued bulk jobs (oldest first). When nothing is
   queued it picks paused jobs that have waited 2 minutes, but only inside sending hours and
   under the daily cap. With nothing to do it sleeps until woken, or for at most a minute.
3. **Engine** (`internal/engine/engine.go`). `RunJob` reconciles anything left mid-flight
   (see [No double sends](#no-double-sends)). It then takes the messages in order, and
   each one waits at the gates before it is submitted.
4. **Gateway** (`internal/gateway/gateway.go`). It makes `POST /message` with one recipient,
   the chosen SIM, a deterministic id, a TTL and a delivery report request. While waiting
   between messages, the engine polls `GET /message/{id}` for in-flight messages.

## The six gates

Every message passes these in order, re-checked in a loop until all pass
(`engine.run.gates`):

1. **Hours.** Outside `SMS_HOURS_START`–`SMS_HOURS_END` the job pauses.
2. **Daily cap.** Segments submitted since midnight (in `SMS_HOURS_TZ`) plus this message
   must not exceed `SMS_LIMIT_PER_DAY`, or the job pauses.
3. **Rolling window.** Segments in the last 15 minutes must leave room, or the engine
   waits until the oldest ones age out.
4. **Spacing.** The gap since the last submission, from any job, is at least a random
   `SMS_PACE_DELAY_MIN`–`MAX`. During warm-up it is `SMS_WARMUP_DELAY`.
5. **Backpressure.** The previous message must have left Pending. If it is stuck there for
   `SMS_BREAKER_STUCK_AFTER`, the job pauses. That usually means Android's "send many
   messages?" prompt or a killed app.
6. **Breaker.** The job stops after `SMS_BREAKER_CONSECUTIVE` failures in a row, or when
   `SMS_BREAKER_RATE` of the last 20 resolved messages failed. Many failures in a row look
   like a carrier block, and continuing would make it worse.

The engine also enforces these:

- **Warm-up.** A bulk job sends its first `SMS_WARMUP_COUNT` messages at the slow warm-up
  pace. It then waits up to `SMS_WARMUP_GATE_TIMEOUT` for all of them to resolve. Any
  failure, or anything unresolved by then, stops the job before it goes to full speed. A
  resumed job that already passed warm-up does a short 5-message warm-up.
- **Phone unreachable.** 3 gateway errors in a row stop the job.
- **HTTP 401.** It stops the job immediately. The credentials are wrong or missing.
- **TTL.** Each message tells the phone to drop it if it is still unsent after
  `SMS_MESSAGE_TTL`, so a phone that comes back after an hour never sends stale texts.

Caps are counted from the **ledger**, an append-only table with one row per submission.
Rows are written *before* each POST, so a request whose response was lost still counts.
The ledger spans all jobs, and rows older than 7 days are pruned at startup.

## No double sends

Each message gets a deterministic gateway id, `<job id>-<index>` (e.g. `k3m9qa-0007`).
`smsd` resends only when SMSGate says it has no message with that id:

- Before the POST, the message is marked `submitting` in the same transaction as the ledger
  row.
- Suppose the POST times out or the process dies. The message stays `submitting`. Before
  anything else is sent, it is looked up by id. If found, the phone's state is adopted. If
  not found, it is queued again. If the lookup fails, the job stops rather than guess.
- If SMSGate answers 409 or "duplicate id", the message was already accepted, so its state
  is looked up and adopted.
- At startup, jobs left `running` by a crash become `paused`. The worker resumes them
  through the same reconcile step.

`RunJob` always reconciles first. On a fresh job that is a no-op.

## Shutdown and cancel

`SIGTERM` (e.g. `systemctl restart`) cancels the worker's context. The message being
submitted finishes and the job is checkpointed as `paused` with reason `interrupted`. The
HTTP server drains for up to 10 s. systemd allows 30 s in total.

Cancel uses the same mechanism for a running job: the worker stops it, then marks its
remaining `queued` messages `cancelled`. For a job that isn't running, cancel is a single
database transaction. The worker claims a job and marks it running under the same lock, so
a cancel always sees it either waiting or running.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/smsd` | Reads env, opens the store, runs `Recover`, wires engine, worker and HTTP server, handles signals |
| `internal/api` | HTTP handlers, request validation, job views, the embedded pages (`web/`), the worker |
| `internal/engine` | Gates, warm-up, breaker, polling, reconcile, quota. The only code that talks to the gateway during a job. |
| `internal/gateway` | SMSGate client (`Client`) and a scriptable `Fake` for tests |
| `internal/store` | SQLite schema and queries |
| `internal/config` | The settings table: names, defaults, ranges, parsing, cross-key rules, hours math |
| `internal/msg` | `Normalize` phone numbers, `Analyze` GSM-7/UCS-2 encoding and segments, `Mask` numbers in logs |

## Database

`/var/lib/smsd/sms.db` (WAL mode, a single connection):

| Table | Rows |
|---|---|
| `jobs` | `id`, `kind` (`send`/`bulk`), `idem_key` (unique when set), `sim`, `state`, `stop_reason`, `warmup_done`, `created_at`, `updated_at` |
| `messages` | Primary key (`job_id`, `row_no`). Also `phone`, `text`, `segments`, `encoding`, `msg_id` (unique, sent to SMSGate), `state`, `error`, `attempts`, `submitted_at`, `resolved_at`, `checked_at` |
| `ledger` | `msg_id`, `sim`, `segments`, `submitted_at`. One row per submission attempt; drives the caps. |

Times are Unix milliseconds. The schema is created on open (`CREATE TABLE IF NOT EXISTS`).
There are no migrations yet. If a column changes, a deploy needs a migration or a fresh
database. Deploys back up the database first.

## Web pages

`internal/api/web/` is embedded with `go:embed` and served as is:

- `index.html` is the sender page: plain HTML and JS with no build step. The browser
  handles the login (Basic auth). The page counts segments as you type (the same GSM-7 rules as
  `internal/msg`), and sends an `Idempotency-Key` with each submit. It refreshes status
  and jobs every 5 s while visible.
- `docs.html` is Swagger UI 5.17.14 from jsDelivr, pointed at `/openapi.json`.
- `openapi.json` is the hand-written OpenAPI 3.1 spec.

## Tests

`go test ./...` needs no phone. `gateway.Fake` scripts the phone's answers: accepted, lost
responses, failures, stuck messages and 401s. A fake clock makes hours of pacing run in
milliseconds. The tests cover:

- **engine:** pacing gaps, the rolling window across jobs, the hours and daily-cap pauses,
  warm-up failure and timeout, the breaker, stuck messages, an unreachable phone, 401,
  lost responses, reconcile on resume, interrupt and resume, and the short warm-up on resume.
- **api:** auth, the three send forms, validation and `skip_invalid`, repeats, idempotency,
  cancel (queued and mid-run), resume, status, the open pages, and spec/route agreement.
- **config, gateway, msg:** env parsing and ranges, the request shape and error mapping,
  number normalization, and segment counting.
