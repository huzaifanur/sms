# API reference

Base URL: your instance, e.g. `https://sms.example.com`. The same reference is machine-readable at
[`/openapi.json`](https://sms.example.com/openapi.json) and browsable at
[`/docs`](https://sms.example.com/docs). The spec lives in
`internal/api/web/openapi.json`. A test (`TestOpenAPIMatchesRoutes`) fails if it and the
server's routes drift apart.

## Authentication

Everything except `/healthz` (the API, the sender page, `/docs` and `/openapi.json`) needs
HTTP Basic auth with `SMS_AUTH_USER` and `SMS_AUTH_PASSWORD`:

```bash
curl -u sms:<password> https://sms.example.com/api/status
# same as: -H "Authorization: Basic $(printf 'sms:<password>' | base64)"
```

A missing or wrong login gets `401` with `WWW-Authenticate: Basic realm="SMS"` and
`{"error": "wrong or missing username and password"}`. A browser asks once when you open the
page, then sends the login with the page's own API calls and with Swagger's "Try it out".

## Conventions

- Requests and responses are JSON. Request bodies are capped at 1 MB. Unknown fields are
  rejected with `400`, so a typo like `"text"` for `"body"` fails loudly.
- Errors look like `{"error": "..."}`.
- **Numbers** may be written `03001234567`, `0300 1234567`, `0300-1234567`,
  `+923001234567`, `923001234567`, `00923001234567` or `3001234567`. They must be Pakistani
  mobiles (10 digits after 92, starting with 3). Responses always show the normalized
  `+923001234567` form.
- **Length:** GSM-7 text fits 160 characters per SMS (153 per part when split). One Urdu
  letter, curly quote or emoji switches the whole message to UCS-2: 70 per SMS, 67 per part.
  The server allows up to `max_segments` SMS per message (3 in production, see
  `GET /api/status`). Every segment counts against the caps.
- **Idempotency-Key** (optional header on `POST /api/send` and `/api/bulk`): any unique
  string. If a job was already created with that key, you get that job back with `200`
  and nothing new is queued. Use it when a client might retry after a timeout. The sender
  page sends a fresh key with every click.

## POST /api/send

One message.

```json
{"to": "03001234567", "body": "Salam, the meeting moved to 5pm."}
```

Returns `202` with a [job](#job) of kind `send`. Single sends go ahead of any queued bulk job,
but they wait for a bulk job that is already running.

## POST /api/bulk

Many messages, in one of two forms. Send one form, not both.

One body for every number:

```json
{"to": ["03001234567", "03007654321"], "body": "Salam, the meeting moved to 5pm."}
```

A body per number:

```json
{"messages": [
  {"to": "03001234567", "body": "Salam Ali, your fee is due Friday."},
  {"to": "03007654321", "body": "Salam Sara, your fee is due Monday."}
]}
```

Optional: `"skip_invalid": true`.

Returns `202` with a [job](#job) of kind `bulk`. At most 1000 messages per request (`413`
above that).

### Validation

Each item is checked before anything is queued:

| Problem | Example error |
|---|---|
| Unparseable or non-mobile number | `bad number "04235761234" (not a Pakistani mobile, expected 03XX XXXXXXX)` |
| Foreign number | `international number "+447911123456" (only +92 is allowed)` |
| Empty or whitespace body | `empty body` |
| Too long | `too long: needs 4 SMS (GSM-7, 500 chars); the limit is 3` |

By default any invalid item rejects the whole request:

```json
422
{"error": "1 invalid message(s); nothing was queued",
 "invalid": [{"index": 3, "to": "0423 5761234", "error": "bad number ..."}]}
```

With `"skip_invalid": true`, the valid items are queued and the invalid ones come back under
`skipped` in the `202` response. If none are valid, it's still `422`.

The same number with the same body twice in one request is sent once. The repeat is listed
under `skipped` (`same number and text as message 1`). The same number with a different body
is sent both times.

`index` is always the item's 1-based position in your request, so you can match results
back to your list.

## GET /api/jobs/{id}

The job with every message:

```json
{
  "id": "k3m9qa",
  "kind": "bulk",
  "state": "running",
  "sim": 1,
  "created_at": "2026-10-22T16:10:02Z",
  "updated_at": "2026-10-22T16:10:02Z",
  "counts": {"delivered": 12, "sent": 2, "pending": 1, "queued": 35},
  "messages": [
    {"index": 1, "to": "+923001234567", "state": "delivered", "segments": 1, "submitted_at": "2026-10-22T16:10:03Z"},
    {"index": 2, "to": "+923007654321", "state": "failed", "segments": 1, "error": "RESULT_ERROR_GENERIC_FAILURE", "submitted_at": "..."}
  ]
}
```

`reason` appears when the job is paused, stopped or cancelled. `skipped` appears only in the
response that created the job.

## GET /api/jobs

The 50 newest jobs, without `messages`: `{"jobs": [...]}`.

## POST /api/jobs/{id}/cancel

Drops every message not yet handed to the phone (they become `cancelled`) and marks the job
`cancelled`. If the job is running, the message being sent finishes first. Returns the job.
`409` if it's already `done` or `cancelled`.

## POST /api/jobs/{id}/resume

Requeues a `stopped` or `paused` job now. Fix the cause in its `reason` first. Messages
that might already be on the phone are looked up there, never resent blindly. Returns the
job. `409` if it's `done` or `cancelled`.

## GET /api/status

```json
{
  "gateway": "ok",
  "sim": 1,
  "hours": "00:00-24:00 Asia/Karachi",
  "in_hours": true,
  "last_15m": "12/150",
  "today": "40/450",
  "max_segments": 3,
  "running_job": "k3m9qa",
  "next_slot_utc": null
}
```

`gateway` is `ok` when SMSGate answers and accepts the credentials. Otherwise it says what's
wrong, e.g. the HTTP 401 message when credentials are missing. `last_15m` and `today` are
segments used / allowed. `next_slot_utc` is when the 15-minute cap frees a slot, or `null`
if one is free now.

## GET /healthz

`ok`. No login needed. It means the process is up, not that the phone is reachable. Use
`/api/status` for that.

## Job

| State | Meaning | What happens next |
|---|---|---|
| `queued` | Waiting for the worker | Starts when the jobs ahead finish |
| `running` | Being sent | |
| `paused` | Hours ended, daily cap reached, a message stuck Pending, or the server restarted | Resumes on its own: 2 minutes after pausing, once inside hours and under the daily cap |
| `stopped` | Circuit breaker, HTTP 401 from SMSGate, phone unreachable, or warm-up failed | Waits for `POST /resume` after you fix the cause |
| `done` | Every message was handed to the phone and resolved, or ran out of time to resolve | |
| `cancelled` | Cancelled by request | |

`done` does not mean every message was delivered. Check `counts` for `failed` and `expired`.

## Message

| State | Meaning |
|---|---|
| `queued` | Not sent yet |
| `submitting` | Being handed to SMSGate. If the server dies here, it looks the message up before ever resending. |
| `pending` | SMSGate has it, not yet on the radio |
| `processed` | SMSGate is sending it |
| `sent` | Left the phone. A delivery report may still arrive. |
| `delivered` | Delivery report received |
| `failed` | The phone or carrier rejected it (`error` says why) |
| `expired` | Still unsent when its TTL (10 min) ran out, so the phone dropped it |
| `cancelled` | The job was cancelled before this one went out |

Delivery reports are polled for about 30 s after a job's last message. A message still
`sent` after that may have been delivered later.
