# Architecture

P1 implements `storage/bbolt`, an independently reusable Fiber Uptime Store.
P2 adds `internal/config`, called directly by `cmd/uptime config check`, alongside
help/version. P3 adds `internal/app` and `serve`: bbolt, plain HTTP, upstream
endpoint probing, built-in dashboard/API, health, and shutdown. The diagram shows
the implemented composition. P4 adds optional upstream Redis persistence; P5 adds
static native TLS and single-user Basic Auth. P5.5 adds loopback defaults, a root
redirect, LibreCosta branding, and Uptime v0.2.2. Archive remains future work.
P6-A pins upstream PR #2535's merge and adds `OpenServiceStorage` for independent
queries and explicit deletion, without constructing Fiber or monitoring workers.

```text
cmd/uptime
    |
    v
internal/app
    |
    +--------------------+
    v                    v
internal/config      Fiber runtime
                         |
                         v
                 Fiber Contrib Uptime
                         |
                         v
                 selected persistence
                    /           \
           Config.Storage    Config.Store
                 |                |
           storage/bbolt    Fiber Storage Redis
                                  |
                            owned go-redis client
```

The CLI loads normalized configuration before constructing the app, which composes
concrete storage and upstream runtime. Future archive operations use the Store query
boundary. The arrows describe responsibilities, not a requirement to wrap every
upstream API in a new abstraction.

## Responsibilities

| Area | Responsibility | Current status |
| --- | --- | --- |
| `cmd/uptime` | Executable entry point, signal context and CLI wiring | Help, version, config check, serve |
| `internal/app` | Fiber composition, lifecycle, health, standalone storage sessions | bbolt or Redis, HTTP/HTTPS, optional Basic Auth; internal history access/removal |
| `internal/config` | Strict YAML, defaults, active environment resolution, validation | Implemented; returns normalized typed config |
| `internal/archive` | Backend-independent service archive/export | Planned |
| `storage/bbolt` | Public implementation of Fiber Uptime's Store contract | Implemented, caller-owned lifecycle |
| `configs` | User-facing example configuration | Implemented; no active secret/env requirements |
| `contracts` | Stable engineering constraints | Present |
| `docs/design` | Design rationale and implementation baselines | Present |
| `docs/decisions` | Significant changes after the initial baseline | Loopback default and first access ADR |

## Invariants

- `cmd` contains CLI wiring, not product/domain logic.
- Public `storage/bbolt` never imports `internal/*`.
- Config parsing is independent of the application runtime; validation must not
  require starting the HTTP application.
- Raw YAML and normalized configuration are separate. Defaults apply only to
  omitted fields; inactive TLS/Auth/storage branches are nil in the result.
  Durations, calendar days, URLs, timezone, and active TLS keypairs are parsed
  before return. Do not repeat defaults, branch selection, or parsing in the app.
- Config check may read active TLS files, but cannot create directories, open
  databases, resolve DNS, connect Redis, bind listeners, or probe endpoints.
- `app.New` accepts normalized bbolt/Redis and TLS/Auth configuration and allocates
  memory only. `Run` is single-use; an already-
  canceled context returns nil without side effects. Open selected storage, Ping,
  and bind before `uptime.New`. Redis preflight uses a five-second context;
  parent cancellation cleans up and returns normally.
- Listener order is raw TCP -> runtimeListener -> optional tls.Listener ->
  Fiber App.Listener. TLS uses the loaded certificate with a TLS 1.2 minimum,
  never rereads files or downgrades to HTTP. Keep the tracked transport inside TLS
  so timeout force-close can unblock a stalled handshake before storage closes.
- Middleware order is Recover -> public health -> GET/HEAD root redirect -> optional Fiber BasicAuth ->
  Uptime -> default 404. Construct BasicAuth before Uptime with a narrow safe panic
  boundary. Its fixed realm is LibreCosta Uptime; it protects only the exact UI path
  and slash descendants. No custom header parser or password verifier.
- Active auth validates one NFC-normalized non-empty username without colon or
  Unicode controls and a bcrypt hash ($2a$/$2b$/$2y$, structure plus bcrypt.Cost).
  Configuration never compares passwords. All four HTTP/HTTPS and public/Auth
  combinations are supported; no extra TLS/auth YAML knobs exist.
- `runtimeStorage` is a small tagged backend with Ping/Close/config mapping.
  bbolt uses `Config.Storage`; Redis uses `Config.Store` and `StorageKeyPrefix`.
  They are mutually exclusive. The app owns the go-redis client with context
  timeouts enabled; Fiber Storage borrows it. Keep Uptime's second Init/Ping.
- `OpenServiceStorage` reuses `openStorage` and its once-only resource cleanup.
  Its three query methods use upstream Store types and safe errors. Redis opens
  through public `uptime.NewRedisStore` with the configured prefix/retention and
  a five-second initialization context; deletion uses `uptime.RemoveService`.
  bbolt uses its existing Store and exclusive file lock. Initialization failures
  release acquired resources. Callers stop operations before closing a session.
  No listener, probe, maintenance task, active/detached policy, or CLI is added;
  `serve` keeps its existing storage mapping and lifetime.
- Mapping into Uptime is pure and explicit, cloning headers/status codes. Do not
  set self-service metadata or reparse/default the normalized configuration.
- Archive logic does not depend on raw bbolt buckets. CLI maintenance commands
  do not manipulate buckets directly.
- Runtime persistence goes through explicit persistence boundaries, with no
  hidden fallback to memory when a backend fails.
- Fiber hooks own Uptime lifecycle. OnListen sets readiness; a shared once-only
  shutdown clears it before `ShutdownWithTimeout`. Uptime's pre-shutdown hook
  cancels/waits workers; HTTP handlers drain before deferred storage Close. Redis
  closes the Fiber handle then the owned client. A private
  listener wrapper synchronizes Serve entry and drains forced-closed connections
  on timeout. Preserve serve/shutdown/close errors together. Constructor/bind
  failures also release resources; never depend solely on GracefulContext.
- Removing an endpoint from configuration never deletes its history.
- Redis provides shared persistence, not leader election or distributed probe
  scheduling. Backend selection never migrates, deletes, merges, or dual-writes data.
- Target availability never determines the Uptime process's readiness.
- Readiness is an atomic flag plus Store.Ping with a one-second context, not a
  duplicate of Uptime's degraded state. Runtime operation failures retain upstream
  policy; open/schema/lock, preflight, and listener bind failures are fatal at startup.
  Redis readiness recovers when Ping succeeds again; no fallback backend exists.
- The bbolt backend owns no goroutines or product policy. Transactions provide
  write serialization; caller callbacks run outside transactions. Its database
  format, scalar encoding, and invariants are validated by persistence tests.

See the [product baseline](../docs/design/v0.1.0-product-technical-design.md)
for planned behavior and [contracts](../contracts/) for stable constraints.
