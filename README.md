# Redis Monitor (Go)

A standalone Redis key browser and metrics dashboard: **one binary** that serves
both the JSON API and the page that consumes it. No PHP, no database, no asset
pipeline, no CDN.

A port of the Redis monitor screen in `branch-based-account-opening-api`, with the
Laravel dependencies replaced by things a single binary can carry: a username and
password (or a static bearer token) instead of Passport, a shared TOTP secret
instead of the `users` table, and a JSON file instead of the application cache.

```
go build -o redis-monitor ./cmd/redis-monitor
cp .env.example .env      # set a username and password, or a token
./redis-monitor
# → http://localhost:8088
```

---

## What it shows

- 🔑 **Key browser** — `SCAN`-paginated, with a `MATCH` pattern and a type filter
- 🗂️ **Namespace dropdown** — the key prefixes that actually exist in the selected
  database, largest first; picking one fills in the matching `MATCH` pattern
- ⏱️ **TTL per key** — with an "expiring soon" highlight under five minutes
- 🧾 **Value preview** — string, hash, list, set, zset and stream, JSON
  pretty-printed
- 📊 **Metrics** — memory, hit rate, ops/sec, clients, evictions, uptime
- 📈 **Four bar charts** — keys by TTL, by type, by prefix, and per database
- 🗓️ **Expiry forecast** — column charts of keys expiring per day and per hour
- 🧮 **Key analytics** — memory and keys by namespace, footprint buckets,
  idle-time (hot/cold) buckets, sampled and estimated keyspace memory
- 📉 **Recorded trend** — keys and memory per day and per hour, plus keys expired
  per day, from points the monitor writes down itself (Redis keeps no history)
- 🗂️ **Breakdown tables** — per-namespace keys/memory/TTL with a one-click
  "browse these keys", and the largest sampled keys
- 🚫 **Sensitive-key redaction** — values for password/token/OTP-ish keys are never
  returned
- 🗑️ **Delete keys** — an explicit key list only, authorised by a **TOTP code**
- 🔌 **Connected clients** — a second page over `CLIENT LIST`: every connection with
  its address, name, database, age, idle time, flags, last command, buffers and
  library, plus per-database / per-name / per-command / idle-time breakdowns
- 🧯 **Bounded by design** — capped `SCAN` iterations, capped previews, cached
  metrics

### It never runs `MONITOR`

Despite the name. `MONITOR` makes Redis broadcast every command to every monitoring
client — roughly a 50% throughput hit, and unsampled, so a server doing 100k
ops/sec floods you with 100k lines/sec. Everything here comes from `INFO`,
`DBSIZE`, bounded `SCAN`, and per-key reads instead.

---

## Exact versus sampled

The distinction runs through the whole payload and the whole UI, because getting it
wrong turns an estimate into a lie.

**Exact.** Memory, hit rate, ops/sec, client counts, keys per database — all from
`INFO` and `DBSIZE`.

**Sampled.** Redis cannot report a TTL, type, prefix, footprint or idle-time
breakdown at all. Those come from a bounded `SCAN` that always reports `scanned`,
`db_keys` and `truncated`, and the page says "sampled N of M keys" whenever the
sample is smaller than the keyspace.

**Recorded.** `INFO` only ever describes right now, so "keys per day" has to be
written down. See [Trend history](#trend-history).

---

## Access control

Two credentials, for two different callers rather than as alternatives:

| | For | Obtained | Expires |
|---|---|---|---|
| **Session token** | a person | `POST /login` with username, password and (optionally) a TOTP code | yes — `REDIS_MONITOR_SESSION_TTL`, sliding |
| **Static token** | scripts, curl, probes | `REDIS_MONITOR_TOKEN` in the environment | never |

Either one alone is enough to boot; with neither the binary **refuses to start**
unless `REDIS_MONITOR_DEV=true`. Both travel the same way —
`Authorization: Bearer …` — and every protected endpoint accepts either.

```env
REDIS_MONITOR_USERNAME=ops
REDIS_MONITOR_PASSWORD=something-long
REDIS_MONITOR_SESSION_TTL=28800                       # 8h idle timeout, slides on use
REDIS_MONITOR_REQUIRE_AUTHENTICATOR_ON_LOGIN=true     # needs REDIS_MONITOR_TOTP_SECRET
```

The page draws whichever form applies: username and password when they are
configured, the token box when they are not. It reads `/config` to find out, which
is why that one endpoint needs no credential.

| Layer | Rule |
|---|---|
| Feature flag | `REDIS_MONITOR_ENABLED=false` → every endpoint 404s |
| Credential | a live session token, or `REDIS_MONITOR_TOKEN` compared in constant time |
| Rate limit | 120 reads/min, 20 deletes/min, **10 logins/min** per IP |
| Authenticator code | deletes always; logins too when `REQUIRE_AUTHENTICATOR_ON_LOGIN=true` |

Details worth knowing:

- **Username and password must be set together.** A half-configured login is a boot
  error, not a form nobody can satisfy.
- **`REQUIRE_AUTHENTICATOR_ON_LOGIN` with no `TOTP_SECRET` is a boot error too**,
  rather than a silent downgrade to password-only.
- **Every login rejection answers identically** — same message, same 401 — whether
  the username or the password was wrong, and both are compared in constant time.
  Telling a caller which half was wrong turns one guess into two cheap ones.
- **Sessions live in memory.** A restart asks everyone to sign in again, which is
  the right trade for never writing bearer material to disk; the static token is the
  credential that survives restarts, and it exists for scripts rather than people.
- **Expiry slides on use**, because a dashboard is meant to be left open — an
  absolute deadline would sign an operator out mid-incident.
- **Signing out revokes server-side** via `POST /logout`, rather than only
  forgetting where the token was written down.

### Setting up the authenticator

```
./redis-monitor -totp-secret
```

prints a fresh secret and an `otpauth://` URI. Put the secret in
`REDIS_MONITOR_TOTP_SECRET` and scan the URI (or type the secret) into any
authenticator app — it is ordinary RFC 6238: SHA1, 6 digits, 30 seconds, ±30s
drift tolerated.

Two consequences worth knowing:

- A **code is single use, across the whole binary.** After it authorises anything
  it is blacklisted for `REDIS_MONITOR_CODE_REUSE_WINDOW` seconds — so a code that
  just let you sign in cannot also authorise a delete, and a delete in the same 30
  seconds waits for the next code. That is the honest reading of "single use", and
  the message says exactly that when it happens.
- With `REDIS_MONITOR_REQUIRE_AUTHENTICATOR=true` and **no secret configured**,
  deletes are refused outright. A misconfiguration is not a licence to delete
  without a code.

---

## Configuration

Everything is read from the environment, with `.env` as a fallback (a real
environment variable always wins). See [`.env.example`](.env.example) for the
annotated full list; the names and defaults match `config/redis-monitor.php` in the
PHP original, so its configuration table transfers unchanged.

The ones you will actually touch:

```env
HTTP_ADDR=:8088
REDIS_MONITOR_USERNAME=         # a person signs in with these two
REDIS_MONITOR_PASSWORD=
REDIS_MONITOR_TOKEN=            # the static credential for scripts
REDIS_MONITOR_TOTP_SECRET=      # required to delete keys
REDIS_HOST=127.0.0.1
REDIS_PORT=6379
REDIS_PASSWORD=null             # "null" is read as "no password"
REDIS_MONITOR_DATA_DIR=data     # where the recorded trend is written
```

At least one of the two credentials must be set, or `REDIS_MONITOR_DEV=true`.
Note `REDIS_MONITOR_PASSWORD` (who may open this dashboard) is a different setting
from `REDIS_PASSWORD` (how it authenticates to Redis).

### No key prefix, deliberately

There is no prefix option and there should not be one. The PHP version needed a
raw client precisely because Laravel's Redis manager prefixes every command, which
double-prefixes the key names `SCAN` hands back. go-redis prefixes nothing, so this
monitor shows exactly what is on the server.

---

## API

Every endpoint answers `{"success": true, "data": {…}}`, the same envelope as the
PHP controller, so payloads from either implementation are directly comparable.

| Method | Path | Notes |
|---|---|---|
| GET | `/` | the page |
| GET | `/healthz` | liveness — no auth, does not touch Redis |
| GET | `/readyz` | readiness — pings Redis |
| GET | `/api/redis-monitor/config` | what the page needs to draw itself — **no credential** |
| POST | `/api/redis-monitor/login` | `{username, password, code}` → a session token — **no credential** |
| POST | `/api/redis-monitor/logout` | revokes the session being used |
| GET | `/api/redis-monitor/session` | whoami: `session`, `token` or `dev` |
| GET | `/api/redis-monitor/overview` | `db`, `fresh` |
| GET | `/api/redis-monitor/stats` | `db`, `days`, `fresh` |
| GET | `/api/redis-monitor/namespaces` | `db`, `fresh` |
| GET | `/api/redis-monitor/keys` | `db`, `pattern`, `type`, `cursor`, `per_page` |
| GET | `/api/redis-monitor/key` | `db`, `key` |
| GET | `/api/redis-monitor/clients` | `db`, `fresh` |
| POST | `/api/redis-monitor/keys/delete` | `{db, keys[], code}` |

Status codes: `401` no/bad token, `403` delete disabled, `404` monitor disabled,
`400` bad request or bad/spent code, `429` rate limited, and **`503` with
`{"success":false,"data":{"reachable":false}}` when Redis is unreachable** — an
operational state this screen exists to report, not a 500 to swallow. `/healthz`
stays `200` in that case: a liveness probe that failed when Redis was down would
restart the monitor exactly when it is most wanted.

### `GET /keys`

`cursor` is opaque — send back whatever the last call returned; `"0"` means the
iteration wrapped around and there is nothing more.

`per_page` is a **target, not a promise.** A `SCAN` batch has to be consumed whole
(the cursor has already moved past it, so a dropped key would never be seen again),
so a page can come back slightly larger than requested. Paging until
`has_more: false` still covers the keyspace exactly once per pass.

`server_filtered: false` means `SCAN … TYPE` was refused — it needs Redis 6 — and
the type filter was applied on this side instead, on a shorter iteration leash.

### `GET /key`

The key travels in the **query string**, not the path: Redis key names routinely
contain `:` and `/`.

A key matching `redact_key_patterns` comes back as `kind: "redacted"` with no value
at all.

### `POST /keys/delete`

```json
{ "db": 0, "keys": ["laravel_cache:config", "laravel_cache:branches"], "code": "418256" }
```

**There is no pattern delete, deliberately.** The caller has to name every key, so
a stray `*` can never wipe a database from this screen. `UNLINK` is used when the
server has it (it frees memory on a background thread), with `DEL` as the fallback
for Redis before 4.0. Every delete writes a log line naming the IP, database and
keys.

---

## Connected clients

`GET /api/redis-monitor/clients` reads `CLIENT LIST`. Exact rather than sampled —
there is no `SCAN` involved — but a snapshot of a list that changes between one
request and the next, cached for `REDIS_MONITOR_CLIENTS_CACHE` seconds and capped at
`REDIS_MONITOR_MAX_CLIENTS` rows.

Server-wide, unlike everything else here: `CLIENT LIST` reports every connection to
the instance, and each row carries the database it happens to be on. The `db`
parameter only picks which connection asks.

Three details that took care:

- **The monitor labels its own connections.** Every connection it opens is named
  `redis-monitor` via `CLIENT SETNAME`, and rows carry `self: true`. Without it an
  operator sees traffic they cannot account for and wonders who is scanning their
  keyspace. The table shows a "this monitor" pill, and a checkbox hides those rows.
- **Field availability is version dependent, and reported.** `laddr`, `tot-mem`,
  `user`, `resp`, `watch` and `lib-name` are all newer than Redis 5. The payload
  carries a `fields` object saying which of them this server actually sent, and the
  UI drops those columns rather than rendering a wall of dashes. Parsing is
  field-name driven for the same reason: an unknown field from a future Redis is
  ignored rather than shifting everything after it.
- **`maxclients` falls back to `CONFIG GET`.** Redis only put it in `INFO` from 7.0.
  Without the fallback the page would report "no maxclients limit" on Redis 5, which
  is wrong — there is a limit, it just cannot be read from `INFO`. If `CONFIG GET`
  is withheld too, the tile says "not reported" rather than claiming there is no
  ceiling.

`CLIENT LIST` is also the one command here an ACL can withhold while leaving the
connection otherwise usable, so its error surfaces on that page rather than being
swallowed into an empty table.

## Trend history

`INFO` and `DBSIZE` only ever describe right now, so "keys per day" cannot be read
out of Redis — it has to be recorded. Every overview and stats build appends one
point (keys, memory, ops, hit rate, clients, expired/evicted counters) to a rolling
series, rate limited to one point per `REDIS_MONITOR_HISTORY_MIN_INTERVAL` seconds.

It is written to `data/history-db{N}.json`, through a temp file and a rename.
**Deliberately not into Redis:** a monitor that wrote keys into the keyspace it
reports on would turn up in its own charts.

- The series starts the first time the page is opened, and the card says
  `recording since …` rather than implying older data exists.
- An hour or day nobody looked at has no reading. Those slots render as a flat
  baseline tick, never as zero keys — `recorded: false` keeps the two apart.
- `expired` / `evicted` per day are deltas of counters that reset on restart; a
  negative delta is reported as `null` rather than a nonsense number.
- Deleting the data directory discards the history. (The PHP original kept this in
  the Laravel cache, where `cache:clear` wiped it; a file survives a restart.)

---

## Cost control

Per key, the overview sample wants `TYPE` and `TTL`, and the stats sample adds
`MEMORY USAGE` and `OBJECT IDLETIME`. Rather than paying a round trip each, **a
whole SCAN batch goes out as one pipelined round trip**, so the cost tracks batch
count rather than key count. The key table costs two round trips per batch: one for
`TYPE`, then one for `TTL`, `MEMORY USAGE` and the type-dependent size command
(`STRLEN`/`LLEN`/`HLEN`/`SCARD`/`ZCARD`/`XLEN`).

On top of that: every sample is capped by both a key limit and an iteration cap,
the overview caches for 15s, the stats payload for 60s, and auto-refresh polls the
metrics every 30s but the stats only every other tick — and **never** the key
table, which would lose the operator's place in the scan.

## Graceful degradation

- `MEMORY USAGE` (Redis 4+) and `OBJECT ENCODING` are optional or renamed on some
  managed Redis offerings; those calls fall back to `null` rather than failing the
  request.
- `OBJECT IDLETIME` errors outright under an LFU eviction policy. Both it and
  `MEMORY USAGE` are probed once against the first sampled key and dropped from the
  batch if refused — an error reply mid-pipeline would cost the whole batch its
  speedup. The charts that needed the missing command then say why they are empty
  (`has_memory` / `has_idle`).
- `SCAN … TYPE` needs Redis 6. On an older server the first attempt errors once and
  the type filter is applied client-side from then on. **Verified end to end against
  Redis 5.0.14.**
- If Redis is unreachable the API answers `503` and the page renders an offline
  banner instead of an empty screen.

---

## Layout

```
cmd/redis-monitor/     wiring, graceful shutdown, -totp-secret
cmd/seed/              writes one key of every type for smoke tests (-clean removes them)
internal/config/       every knob, plus a dependency-free .env reader
internal/redisx/       per-database client pool, INFO parser, refusable commands
internal/monitor/      the read model: overview, namespaces, keys, detail, stats, history, clients, delete
internal/httpapi/      routes, login and sessions, rate limiting, JSON envelope
internal/totp/         RFC 6238, and the single-use code blacklist
internal/web/          the embedded single-page UI
```

One third-party dependency: `github.com/redis/go-redis/v9`. Everything else is
stdlib — HTTP routing, TOTP, `.env` parsing, rate limiting, and the charts, which
are plain CSS-height divs rather than a charting library.

## Development

```
go test ./...                       # unit tests, no Redis needed
go vet ./... && gofmt -l .

go run ./cmd/seed                   # fixtures under monitorseed:
go run ./cmd/seed -clean            # remove them again
go run ./cmd/redis-monitor
```

The seeder writes a string, a JSON string, a list, set, zset, hash and stream, a
key expiring in four minutes, a `user:token:…` key that must come back redacted,
and a value with an `access_token` inside it that value-level redaction must mask.

### If `go test ./...` reports "An Application Control policy has blocked this file"

That is the machine, not the tests. `go test` compiles each package to a randomly
named `.exe` under a temp directory and runs it; where WDAC or Smart App Control is
enforcing, the first execution of an unseen binary is refused and the package
reports `FAIL` for a reason unrelated to the code.

`scripts/test.ps1` works around it by building each package to a stable, explicitly
named binary and retrying the refused first run:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\test.ps1
```

On any machine without that policy, plain `go test ./...` is equivalent.

## Docker

```
docker build -t redis-monitor .
docker run --rm -p 8088:8088 \
  -e REDIS_HOST=host.docker.internal \
  -e REDIS_MONITOR_TOKEN=... \
  -e REDIS_MONITOR_TOTP_SECRET=... \
  -v redis-monitor-data:/data \
  redis-monitor
```

The volume is only needed if the recorded trend should outlive the container.
