# Operations

## Phone setup

The phone runs [SMSGate](https://sms-gate.app) and connects out to its cloud relay. Do this
once:

- [ ] Install SMSGate. Switch on **Cloud Server**. Local Server can stay on too.
- [ ] Copy the Cloud Server **username** and **password** shown in the app into
      `/etc/smsd/smsd.env` (see [Configuration](#configuration)), then restart.
      `GET /api/status` should report `"gateway": "ok"`. The sender page shows
      **Phone connected**.
- [ ] Raise Android's SMS limit (by default it allows 30 per 30 minutes, then asks on
      screen). Connect over ADB and run:
      `adb shell settings put global sms_outgoing_check_max_count 500` and
      `adb shell settings put global sms_outgoing_check_interval_ms 1800000`
- [ ] Exclude SMSGate from battery optimisation, allow autostart, and keep the phone
      charging and online.
- [ ] In SMSGate set a backstop in case `smsd` misbehaves: delay 5–8 s, limit 10/min, FIFO.
- [ ] Send one message to a number you own. If it arrives from the wrong SIM, set
      `SMS_SIM=2`.

## Server

Any Linux host with nginx, certbot and Python 3 (developed on Ubuntu 24.04, arm64). Set
`SMSD_SERVER` (e.g. `ubuntu@your-server`) and `SMSD_DOMAIN` (e.g. `sms.example.com`) when deploying.

| What | Where |
|---|---|
| Binary and releases | `/opt/smsd/releases/<UTC timestamp>/`, `/opt/smsd/current` → the live one. Last 5 kept. |
| Service | `smsd.service` (user `smsd`, hardened: `ProtectSystem=strict`, `ProtectHome`, `NoNewPrivileges`) |
| Settings and secrets | `/etc/smsd/smsd.env` (root, 0600). Created on the first deploy, never overwritten. |
| Database | `/var/lib/smsd/sms.db`. Before each deploy it is backed up to `sms.db.<release>.backup`. |
| Listen address | `127.0.0.1:8097` (only nginx reaches it) |
| nginx site | `/etc/nginx/sites-available/$SMSD_DOMAIN` (HTTP → HTTPS, proxy to 8097, 1 MB body limit) |
| TLS | Let's Encrypt, `/etc/letsencrypt/live/$SMSD_DOMAIN/`. Renewed by `certbot.timer`. |
| DNS | an A record for `$SMSD_DOMAIN` pointing at your server |

## Configuration

All settings are environment variables. Production values live in `/etc/smsd/smsd.env`,
except `SMS_LISTEN` and `SMS_DATA_DIR`, which the systemd unit sets. Change one like this:

```bash
ssh $SMSD_SERVER
sudo nano /etc/smsd/smsd.env
sudo systemctl restart smsd
```

Out-of-range values stop the service from starting. Read the reason with
`journalctl -u smsd -n 20`.

| Variable | Default | Allowed | Production |
|---|---|---|---|
| `SMS_AUTH_USER` | required | text | `sms` |
| `SMS_AUTH_PASSWORD` | required | 12+ characters | generated on the first deploy |
| `SMS_GATEWAY_URL` | required | http(s) URL | `https://api.sms-gate.app/3rdparty/v1` |
| `SMS_GATEWAY_USERNAME` / `SMS_GATEWAY_PASSWORD` | none | text | from SMSGate's Cloud Server screen |
| `SMS_GATEWAY_TIMEOUT` | 10s | 2s–60s | |
| `SMS_SIM` | required | 1 or 2 | 1 |
| `SMS_COUNTRY_CODE` | 92 | 1–3 digits | |
| `SMS_HOURS_START` / `SMS_HOURS_END` | 10:00 / 20:00 | HH:MM, start before end; `24:00` allowed as end | 00:00 / 24:00 (always open) |
| `SMS_HOURS_TZ` | Asia/Karachi | IANA zone | |
| `SMS_PACE_DELAY_MIN` / `MAX` | 6s / 9s | ≥ 3s, min ≤ max | |
| `SMS_LIMIT_PER_15M` | 150 | 1–180 segments | |
| `SMS_LIMIT_PER_DAY` | 450 | 1–500 segments | |
| `SMS_WARMUP_COUNT` | 15 | 5–50 | |
| `SMS_WARMUP_DELAY` | 15s | ≥ pace max | |
| `SMS_WARMUP_GATE_TIMEOUT` | 3m | 1m–10m | |
| `SMS_BREAKER_CONSECUTIVE` | 3 | 1–10 | |
| `SMS_BREAKER_RATE` | 20% | 5%–50% of the last 20 | |
| `SMS_BREAKER_STUCK_AFTER` | 60s | 30s–300s | |
| `SMS_MESSAGE_MAX_SEGMENTS` | 1 | 1–3 | 3 |
| `SMS_MESSAGE_TTL` | 10m | 1m–60m | |
| `SMS_POLL_INTERVAL` | 5s | 2s–30s | |
| `SMS_LISTEN` | 127.0.0.1:8097 | host:port | set in the unit |
| `SMS_DATA_DIR` | ./data | directory | `/var/lib/smsd` (unit) |

Durations accept `6s`, `3m`, `1m30s` or a bare number of seconds.

To change the login, edit `SMS_AUTH_USER` or `SMS_AUTH_PASSWORD`, restart, and update
any stored copies of the login and anything else that calls the API.

## Deploy

```bash
deploy/deploy.sh
```

On the dev machine, it runs `go test ./...` and cross-builds a static `linux/arm64` binary.
It uploads the binary with the unit file and nginx site. It then runs `deploy/activate.sh`
as root on the server, which:

1. creates the `smsd` system user if missing
2. unpacks the release into `/opt/smsd/releases/<timestamp>/`
3. on the first deploy only, writes `/etc/smsd/smsd.env` with the login `sms` and a random
   password. It also replaces the bearer token of releases before 2026-10-22 with that
   login.
4. backs up the database
5. on the first deploy only, issues the TLS certificate
6. points `/opt/smsd/current` at the release, installs the unit, and restarts `smsd`
7. waits up to 20 s for `127.0.0.1:8097/healthz`. If it doesn't answer, it prints the log and
   rolls back to the previous release and unit
8. installs the nginx site and runs `nginx -t` (restoring the old site and rolling back if
   it fails), then reloads nginx
9. confirms `https://$SMSD_DOMAIN/api/status` answers `401`, which proves the
   login check is in front of the API
10. prunes releases beyond the newest 5

Manual rollback:

```bash
ssh $SMSD_SERVER
ls /opt/smsd/releases
sudo ln -sfn /opt/smsd/releases/<older> /opt/smsd/current && sudo systemctl restart smsd
```

Restarting is safe at any time. A running job is checkpointed as `paused` and resumes
about 2 minutes after the service is back.

## Logs

```bash
journalctl -u smsd -f
```

The engine logs one line per message with the number masked (`+92300****567`). It also logs
warm-up progress, cap waits, and every pause or stop with its reason.

## When a job pauses or stops

The job's `reason` (on the sender page, or `GET /api/jobs/{id}`) says what happened.

| Reason contains | State | Do |
|---|---|---|
| `outside sending hours` | paused | Nothing. It resumes when the window opens. |
| `daily cap reached` | paused | Nothing. It resumes after midnight Asia/Karachi. |
| `has been Pending for` | paused | Check the phone for Android's "allow sending many messages?" prompt, or a killed SMSGate. It retries on its own every 2 minutes. |
| `interrupted` | paused | The service restarted. It resumes on its own. |
| `HTTP 401` | stopped | Set the gateway username and password in the env file, restart, then **Resume**. |
| `phone unreachable` | stopped | The phone is offline or SMSGate is dead. Bring it back, check `/api/status`, then **Resume**. |
| `warm-up failed` / `circuit breaker` | stopped | Messages are failing: no balance, a SIM block, or bad numbers. Check the failed messages' `error` and send a test from the phone itself. **Resume** only once that works. |
| `cannot confirm whether` | stopped | A message's fate is unknown and the phone couldn't be asked. Once the phone answers, **Resume** looks it up before sending anything else. |

`GET /api/status` (or the chips on the sender page) shows the gateway check and how much
of the 15-minute and daily caps is used.
