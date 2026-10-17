# Compatibility Contract

## Current Surface

The executable exposes help (`uptime`, `help`, `-h`, `--help`), version
(`version`, `--version`), `config check [--config PATH]`, `serve [--config PATH]`,
`service list [--config PATH]`, and `service export <id> [--config PATH] [--output PATH]`.
Configuration commands default to `./uptime.yaml` and have `-h`/`--help`.
There is no global `--config`.
Bare `config`, unexpected arguments, and unknown commands/options are usage
errors. Bare `service` shows its help. CLI service removal remains unimplemented.

Successful output goes to stdout; diagnostics go to stderr. Help, version, and
valid configuration return 0; usage errors return 2; invalid/unreadable YAML,
missing active env, invalid TLS, and output failures return 1. Successful config
check prints only `configuration is valid` with a newline and empty stderr.
Config failure has empty stdout and a safe `config check: <error>` diagnostic.
No configuration is dumped. See source/tests for concrete help text.

P1 adds the public `storage/bbolt` package: `Config`, `Store`, `Open`, the upstream
Store methods, and `Name`, `Ping`, `RemoveService`, `Close`. Raw database/bucket
access is private. P2's `internal/config.LoadFile` adds the product configuration
contract; it does not open storage or construct a standalone runtime.
P6-A adds only an internal standalone storage session, not a new CLI command or
public SDK. It provides existing Store query types, explicit removal and Close.
The module is `github.com/librecosta/uptime`; its Go baseline is 1.26. Go support
follows the active Fiber major version's supported window; CI covers 1.26.x and
1.27.x. Updating that window requires coordinated module, CI, and documentation
changes.

## YAML Configuration (P2)

The [official example](../configs/uptime.example.yaml) shows the supported keys.
The root sections are `server`, `storage`, `uptime`, `ui`, `auth`, and `endpoints`.
YAML v3 decoding is strict throughout, including inactive branches: unknown and
duplicate keys, multiple documents, wrong scalar/container types, explicit null,
and merge keys fail. Use omission for defaults and `""`/`[]`/`{}` for intentional
empty values where allowed. Ordinary correctly typed anchors/aliases are supported.

Raw YAML retains omission state separately from normalized `Config`. The loader
applies omitted-field defaults, selects active branches, substitutes allowed env
values, and parses/validates the result. Dependent endpoint defaults use the
effective parsed interval. No later runtime code needs to apply defaults or
reparse duration/URL/timezone values. URLs use `*url.URL`, durations use
`time.Duration`, retention/window use `CalendarDays`, timezone uses
`*time.Location` (with embedded tzdata), and enabled TLS carries a loaded keypair.
TLS/Auth disabled branches are nil; exactly the selected storage branch is non-nil.

### Frozen Defaults

| Field | Omitted value |
| --- | --- |
| `server.address` | `127.0.0.1:18580` |
| `server.shutdown_timeout` | `10s` |
| `server.tls.enabled`, `auth.enabled` | `false` |
| `storage.type` | `bbolt` |
| `storage.bbolt.path` | `./data/uptime.db` |
| `storage.redis.key_prefix` | `fiber:uptime` |
| `uptime.interval` | `10s` |
| `uptime.retention`, `uptime.window` | `90d`, `30d` |
| `uptime.timezone` | `UTC` |
| `ui.path`, `ui.title` | `/uptime`, `Service Status` |
| `ui.description` | `Current service availability.` |
| `ui.footer` | `Powered by LibreCosta Uptime.` |
| `ui.favicon_url` | empty, built-in favicon |
| `ui.thresholds.green`, `ui.thresholds.yellow` | `0.999`, `0.99` |
| endpoint `name`, `method` | its literal ID, `GET` |
| endpoint `interval`, `timeout` | global interval, `min(5s, effective interval)` |
| endpoint `description`, `headers`, `expected_status_codes` | empty |

Explicit empty required values are errors, never replaced by defaults.
P5.5 changes the omitted listener from `:8080` to loopback `127.0.0.1:18580`.
Explicit addresses retain their syntax and behavior. Public, Docker, and remote
proxy access can use `:18580` explicitly; a local proxy may keep loopback.
See [ADR 0001](../docs/decisions/0001-local-first-startup.md).

### Environment and Active Branches

Only `${VAR_NAME}` is substituted, with `[A-Za-z_][A-Za-z0-9_]*` names. Expansion
is one-pass; substituted values are not rescanned. Missing active variables fail;
existing empty variables substitute successfully and then face field validation.
Malformed `${...}` expressions fail. Other dollar signs remain literal, including
`$VAR`, `$(command)`, and bcrypt `$2...`; no shell/default-expression evaluation exists.

Allowed fields: server address/shutdown timeout, active TLS paths, active bbolt
path or Redis URL/prefix, uptime interval/retention/window/timezone, UI strings,
active auth username/hash, endpoint name/description/URL/interval/timeout, and
header values. Storage type, endpoint ID/method, header names, booleans, numbers,
and list/container structure cannot interpolate. Inactive TLS/Auth/storage values
are not expanded or semantically validated and do not survive normalization.

### Validation and Side Effects

| Area | Contract |
| --- | --- |
| Server | host:port; numeric port 1..65535; shutdown timeout > 0 |
| TLS | enabled paths required/readable; `tls.LoadX509KeyPair` must succeed; disabled paths ignored |
| Storage | literal `bbolt` or `redis`; bbolt path non-empty; Redis URL has redis/rediss scheme and host, no fragment, and passes pinned go-redis ParseURL |
| Redis prefix | non-empty after env expansion; no leading/trailing colon or whitespace; no control characters; internal colons allowed |
| Auth | one required NFC-normalized username without colon/Unicode controls; bcrypt-only hash ($2a$/$2b$/$2y$), structure plus bcrypt.Cost validation; no plaintext field |
| Uptime | interval >= 1s; canonical positive integer `Nd` calendar spans without leading zeros or overflow; window <= retention; Redis retention <= 106750d (no such limit for bbolt); UTC/IANA/Local timezone |
| UI | clean absolute non-root path, no trailing slash, no `/livez` or `/readyz`; non-empty title; finite 0 < yellow <= green <= 1 |
| UI text | description/footer must be non-empty; omission uses LibreCosta defaults; favicon may be empty, otherwise root-relative or absolute HTTP(S) URL |
| Endpoint identity | at least one for serve/config check; maintenance also allows explicit `endpoints: []`; unique literal `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` IDs; non-empty name |
| Endpoint request | literal uppercase GET/HEAD; http/https URL with host, no userinfo/fragment, query allowed |
| Endpoint timing | interval >= 1s; 1ms <= timeout <= effective endpoint interval |
| Expected codes | unique 100..599; empty means the upstream 2xx/3xx policy |
| Headers | valid literal token names, canonicalized, case-insensitive duplicates rejected; no Host or CR/LF values |

P2 explicitly tightens the initial design's UI path handling: invalid paths are
rejected, not silently normalized. It selects stable YAML v3 instead of the
initial provisional v4 choice. The original design remains the broader roadmap.
P3 corrects description/footer explicit-empty handling: Fiber Uptime v0.2.0
would replace empty strings with its defaults, so the loader rejects them.
Empty favicon remains legal. The P3 audit also aligns endpoint timeout validation
with Fiber Uptime's 1ms minimum. P4 adds offline go-redis URL parsing and prefix
validation. URL credentials, database paths, and go-redis-supported query options
are allowed; parser errors never echo the URL. No extra Redis YAML knobs exist.

Relative bbolt and TLS paths retain process-CWD semantics, not YAML-directory
semantics. Validation only reads the YAML and active TLS files. It never makes
directories, opens/locks bbolt, resolves DNS, connects Redis, probes endpoints,
binds ports, or constructs Fiber/Uptime. Errors identify fields/categories without
printing Redis credentials, hashes, header values, secret URL queries, TLS keys,
or environment values. Do not implement config stringification or dumps.

## Service List and Archive v1 (P6-B1)

`service list/export` use `LoadServiceFile`, which permits only the additional case
of explicit `endpoints: []`. Omitted or null endpoints remain invalid. All other
strict/active validation and CWD path rules are unchanged. Help opens no config or
storage. Options may precede or follow the export ID; `--output` is export-only.
There is no `--force`, `--yes`, `--online`, or CLI deletion command.

The list contains only persisted services, sorted lexically by ID, with columns
`ID`, `STATUS`, `LAST SEEN (UTC)`. Status is lowercase `active` iff the ID is in
current YAML, otherwise `detached`; neither is a liveness or writer-exclusion claim.
Times are UTC RFC3339, zero is `-`, and an empty store prints only the header.
IDs containing invalid UTF-8, whitespace or non-printable characters use ASCII escapes.

Archive v1 has these stable JSON fields (no backend-dependent shape):

| Object | Fields |
| --- | --- |
| Root | `format` = `librecosta-uptime-service-archive`, integer `version` = 1, `application_version` from the CLI build, `exported_at`, `timezone`, `service`, `daily`, `current_day` |
| `service` | string `id`, `name`, `description`, `sample_interval`; UTC RFC3339 `created_at`/`last_seen_at` omitted only when zero |
| `daily[]` | local date `day`, integer `up_slots`/`expected_slots`, boolean `finalized` |
| `current_day` | local date `day`, integer `up_slots`/`expected_slots_so_far` |

Export time is injected and formatted UTC RFC3339; the timezone is the normalized
configuration's location name. An empty description stays `""`; empty daily history
is `[]`. Persisted sample intervals >=1s win; otherwise use the global YAML interval,
rendered as a Go duration string. Existence is checked before history queries.
Daily queries select one ID with no bounds, preserve all real counts/finality
(including any daily row for today), sort ascending by day, and never fill missing
dates or run maintenance. Today's raw up count is zero if absent and never clamped.

Expected current-day slots are theoretical: from local midnight to the start of
the current slot, subtract the floor-aligned slot of max(midnight, CreatedAt),
clamped at zero. Use actual elapsed duration and calendar next-midnight (`AddDate`)
for DST; future creation gives zero. LastSeenAt does not reduce this count. Daily
rows and current raw samples are distinct sources, not additive history. There is
no cross-query Redis snapshot guarantee or import/restore facility.

Only whitelisted service/history fields are serialized: no configuration, endpoint
URLs, headers, auth/TLS data, Redis URLs, raw slot IDs or instance/PID/hostname.
Queries and storage Close finish before success output or publication. Missing
services, query/close failures and output errors return 1 with safe stderr; usage
errors return 2; success/help return 0. No success text is emitted on failure.

Default output is `uptime-service-YYYYMMDDTHHMMSSZ.json` in CWD, independent of ID.
Explicit paths also use CWD. UTF-8 JSON is indented with a trailing newline.
Write to an owned same-directory 0600 temporary file, Sync, Close, then hard-link
to the final name without replacement and remove the temporary name. Never create
parents or replace files/symlinks; unsupported filesystems fail safely. Windows
permissions follow directory ACLs. A failure after publication may leave a complete
archive (for example, stdout or temporary-name cleanup failure); it never truncates
an existing target. No direct-to-final or unconditional rename fallback exists.

bbolt maintenance uses an existing file with an exclusive lock and never creates
a new file or parent. `Config.MustExist` enforces this at open, avoiding a Stat/create
race; ordinary `Open` and `serve` still initialize new databases. Stop serve before
bbolt management; Redis may be queried online. Neither command starts HTTP/probes
or writes source history, and changing YAML never implicitly deletes history.

## Standalone Runtime (P3–P5.5)

`serve` loads normalized configuration and accepts bbolt or Redis, HTTP or HTTPS,
and optional single-user Basic Auth. `rediss://` secures the outbound Redis
connection independently of native server HTTPS.
No host/port/storage/tls command-line overrides or fallback exist.

Active server TLS means HTTPS only, with a TLS 1.2 minimum and the static keypair
already loaded by config.LoadFile. Run does not reread cert/key files. The raw TCP
listener is tracked before tls.NewListener wraps it; Fiber serves that supplied
listener. Invalid active TLS fails configuration loading and never downgrades to
HTTP. No ACME, mTLS, hot reload, multi-cert SNI or TLS tuning YAML is provided.

Active auth resolves env, normalizes the username to NFC, and validates username
and bcrypt hash before returning Config. Hash structure is
`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`; bcrypt.Cost validates the cost without
password comparison or an additional product cost policy. Disabled auth skips env
and semantic validation and leaves Auth nil.

Fiber official BasicAuth protects exactly `ui.path` and paths starting with
`ui.path + "/"`. For `/status`, `/status2` and `/status-page` stay outside auth.
Health always remains public. Missing/wrong credentials receive Fiber's 401 Basic
challenge with fixed realm `LibreCosta Uptime`, `Cache-Control: no-store`, and
`Vary: Authorization`; the response body is not a product ABI. Correct credentials
permit the dashboard/API; authenticated unknown descendants remain 404.
BasicAuth constructor rejection becomes the safe operational error
`cannot initialize Basic Auth`, before Uptime workers start.

HTTP+Auth is supported for trusted TLS proxy/private-network use. Basic Auth does
not encrypt credentials; untrusted exposure requires native TLS or a trusted TLS
proxy. Hash non-ASCII passwords after NFC normalization to match Fiber. No custom
authorizer, plaintext password, multiple users, sessions, RBAC or OAuth exists.

Serve help and normal requested SIGINT/SIGTERM/context cancellation return 0.
Invalid config, unsupported capabilities, storage open/schema/lock failures, bind
failures, unexpected Listener return (including nil), shutdown/close errors, and
output failures return 1. Usage errors return 2. Operational errors use a safe
`serve: <error>` stderr diagnostic; runtime does not dump config or secrets.
Lifecycle logging uses Fiber's official logger on stderr, with no startup banner.
The successful OnListen log identifies LibreCosta Uptime, the bound listener,
HTTP/HTTPS, and the configured dashboard path. HTTP on loopback prints its actual
local URL. Wildcard listeners use a host placeholder; HTTPS always uses a
certificate-host placeholder and asks for a certificate-covered hostname/IP,
without assuming localhost is valid. Logs never include endpoint/Redis URLs,
request headers, auth credentials, or the normalized configuration.

| Route | Exact response |
| --- | --- |
| GET `/livez` | 200, `ok\n`, no storage/target/readiness access |
| GET `/readyz` | 200, `ready\n` iff listening readiness flag and Store.Ping succeeds; otherwise 503, `not ready\n` |

Both health routes set `Content-Type: text/plain; charset=utf-8` and
`Cache-Control: no-store`. Ping receives a one-second timeout context; bbolt's
documented blocked-I/O cancellation limits still apply. Failures expose no details
and produce no per-request error log. Target DOWN never changes readiness.
The configured `ui.path` and `<ui.path>/api/status` are upstream dashboard/API
routes. GET/HEAD `/` returns HTTP 302 to the validated `ui.path`, without copying the
incoming query. The redirect is public; the target dashboard/API still requires
Basic Auth when enabled. Other root methods and unmatched paths remain 404. Only YAML endpoints
are monitored; no self service is manufactured.

Startup opens selected storage, completes preflight Ping, and binds before
constructing Fiber Uptime. Redis preflight has a five-second timeout with go-redis
context timeouts enabled. Failure is fatal with a safe `cannot connect Redis storage`
diagnostic; parent cancellation cleans up and returns normally unless cleanup fails.
Keep upstream's second Redis Init/Ping: failures after preflight use its recoverable
degraded behavior. Redis outages leave liveness 200 and readiness 503; readiness
returns to 200 after Ping succeeds, without restarting or switching backend.

Shutdown clears
readiness, runs Fiber shutdown/Uptime worker cancellation and waiting, drains HTTP,
then closes bbolt or the Fiber Redis handle followed by its owned go-redis client.
Timeout errors are retained: active connection I/O is forced
closed and handlers must finish before storage is released. Shutdown timeout is
not a promise to forcibly interrupt blocked storage I/O. After initialization,
operation failures follow upstream degraded behavior without automatic exit.

Selecting bbolt versus Redis only selects an active persistence source. There is
no migration, merge, dual-write, fallback, or deletion of the old backend's data.
Shared Redis persistence provides neither leader election nor distributed
scheduling: each process probes its configured endpoints independently.

Endpoint timeout must be at least 1ms and no greater than its effective interval.
Both config check and serve reject smaller values during configuration loading,
before runtime construction. Upstream constructor rejections are still converted
from panic into safe operational errors.

## Compatibility-Sensitive Boundaries

As implemented and released, preserve the semantics of:

- Public Go APIs and their lifecycle/error behavior.
- CLI commands, flags, output semantics, and exit behavior.
- Implemented YAML keys, defaults, environment interpolation, and normalization.
- `/livez`, `/readyz`, and product-exposed Uptime dashboard/API routes.
- Archive JSON formats and documented defaults.
- Persistent schema migration behavior.

This list also constrains future archive work. Concrete APIs and formats belong
in source, tests, and their specific
documentation, not duplicated here.

## Pre-1.0 Evolution

- v0.x may evolve, but prefer additive changes whenever practical.
- Patch releases (for example, v0.1.1 to v0.1.2) must not intentionally introduce
  breaking user-facing changes.
- Minor releases (for example, v0.1 to v0.2) may break compatibility before v1.0
  only intentionally, with documentation, tests, and migration guidance where
  applicable.
- Released persistent data must not become unreadable without a supported
  forward migration. Pre-1.0 status is not permission to discard user history.
