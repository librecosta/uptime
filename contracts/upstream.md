# Upstream Contract

P6-A uses Fiber v3.6.1, Fiber Storage Redis v3.6.2, go-redis v9.23.0,
bbolt v1.5.0, stable `go.yaml.in/yaml/v3` v3.0.5, x/crypto v0.58.0 (bcrypt)
and x/text v0.43.0 (username NFC). `go.mod` is authoritative. No CLI/config
framework is used.

## P6-A upstream integration

Fiber Uptime is pinned to merged [PR #2535](https://github.com/gofiber/contrib/pull/2535)
commit `e811012f822675810017efdf00823849f97f2323`, resolved by Go to
`v0.2.4-0.20261017105741-e811012f8226`. This is a temporary commit pin until a
released module version includes the API; there is no fork, floating main or replace.
Running `go get` for that commit and `go mod tidy` selected only these changes:

| Module | Before | After |
| --- | --- | --- |
| `github.com/gofiber/contrib/v3/uptime` | v0.2.2 | v0.2.4-0.20261017105741-e811012f8226 |
| `github.com/gofiber/fiber/v3` | v3.5.0 | v3.6.1 |
| `github.com/gofiber/storage/redis/v3` | v3.6.1 | v3.6.2 |
| `golang.org/x/crypto` | v0.57.0 | v0.58.0 |
| `golang.org/x/text` | v0.42.0 | v0.43.0 |
| `github.com/gofiber/utils/v2` | v2.6.1 | v2.6.2 |
| `github.com/klauspost/compress` | v1.20.1 | v1.20.3 |
| `github.com/molecule-man/go-brrr` | v1.2.0 | v1.3.0 |
| `golang.org/x/net` | v0.59.0 | v0.61.0 |
| `golang.org/x/sys` | v0.48.0 | v0.49.0 |

The module graph includes upstream's Testcontainers Redis v0.44.0 test dependency
and its Docker/telemetry dependencies; none is imported by product or product
test code. Existing opt-in Redis tests and CI remain the verification path.
Other direct dependency versions and the Go support window are unchanged.

Compared the downloaded sources with v0.2.2: the public Store interface,
dashboard, Redis schema/algorithms and borrowed-client ownership are unchanged.
NewRedisStore initializes the native Store with configured prefix and retention,
without a Fiber App, workers or maintenance. Redis TTL overflow validation is
shared with the middleware at Redis configuration construction; custom Stores
remain unrestricted by that Redis bound. Other Uptime changes modernize map,
slice, min/max and worker wait-group syntax without changing these contracts.

Fiber v3.6.1 snapshots shutdown hooks under its mutex and drains outside that
mutex; the product still owns shutdown coordination and the tracked TLS listener.
BasicAuth tightens malformed duplicate Authorization header handling and preserves
existing Vary headers; single-user bcrypt verification and the fixed realm remain
native Fiber behavior. Fiber Storage Redis's implementation is unchanged from
v3.6.1. Existing HTTP/TLS/auth/shutdown, original-schema and race tests cover the
product boundaries; real Redis tests cover both runtime and independent access.

`OpenServiceStorage` reuses the runtime resource owner, but never starts `serve`.
Queries use the public Store API; Redis deletion uses public `uptime.RemoveService`.
Initialization has a five-second context, and errors retain causes behind safe
operation labels. Context timeouts depend on client/I/O behavior, not forced
cancellation. Close releases the Fiber handle then its owned client exactly once.

## P5.5 upgrade review

Compared the downloaded v0.2.0 and v0.2.2 module sources and the
[v0.2.2 release](https://github.com/gofiber/contrib/releases/tag/v3%2Fuptime%2Fv0.2.2).
Only the dashboard template, UI tests, README, and module files differ. Public
`storage.Store`, native Redis storage, worker shutdown, Go snapshot/JSON API,
and Service Insights calculations are unchanged. Fiber itself remains v3.5.0.
The bbolt adapter needs no API or schema changes. Fiber Storage Redis v3.6.0 to
v3.6.1 changes only module files; borrowed-client ownership remains unchanged.
TLS still wraps the tracked listener and BasicAuth still protects the UI namespace.

The upstream dashboard adds resilient polling (at least ten seconds, paused in
hidden tabs, immediate refresh on return), STALE/ERROR feedback, and accessibility
and touch improvements to Insights. Adopt that native UI without extra queries,
wrappers, frontend dependencies, or persistent data. Browser interaction remains
a manual acceptance item alongside automated dashboard/API and security checks.

`go get github.com/gofiber/contrib/v3/uptime@v0.2.2` followed by `go mod tidy`
selects these changes through the upstream minimum-version requirements:

| Module | Before | After |
| --- | --- | --- |
| `github.com/gofiber/contrib/v3/uptime` | v0.2.0 | v0.2.2 |
| `github.com/gofiber/storage/redis/v3` | v3.6.0 | v3.6.1 |
| `github.com/redis/go-redis/v9` | v9.22.0 | v9.23.0 |
| `github.com/gofiber/schema` | v1.8.6 | v1.8.8 |
| `github.com/gofiber/utils/v2` | v2.5.1 | v2.6.1 |
| `github.com/klauspost/compress` | v1.20.0 | v1.20.1 |
| `github.com/mattn/go-colorable` | v0.1.15 | v0.1.16 |
| `github.com/molecule-man/go-brrr` | v1.0.1 | v1.2.0 |
| `github.com/tinylib/msgp` | v1.6.4 | v1.6.5 |
| `github.com/valyala/fasthttp` | v1.74.0 | v1.75.0 |
| `go.uber.org/atomic` | v1.11.0 | v1.12.0 |

Other direct dependencies and the Go/CI support window are unchanged. The selected
modules' test dependency graph also changes these go.sum-only versions:

| Module | Before | After |
| --- | --- | --- |
| `github.com/containerd/log` | v0.1.0 | v0.2.0 |
| `github.com/ebitengine/purego` | v0.10.2 | v0.11.1 |
| `github.com/fxamacker/cbor/v2` | v2.9.3 | v2.9.4 |
| `github.com/klauspost/cpuid/v2` | v2.2.10 | v2.4.0 |
| `github.com/magiconair/properties` | v1.18.11 | v1.18.12 |
| `github.com/moby/moby/api` | v1.55.0 | v1.56.0 |
| `github.com/moby/moby/client` | v0.5.1 | v0.6.0 |
| `github.com/moby/sys/userns` | v0.2.0 | v0.2.1 |
| `github.com/shamaton/msgpack/v3` | v3.2.2 | v3.2.3 |

These do not add application imports. No full dependency update, replace directive, or upstream source patch
is used. Verify with the existing runtime/storage suites, race scope, and opt-in
real Redis integration job; the original-schema fixture opens pre-branding data.

## Integration boundaries

| Upstream | Role | Important assumption |
| --- | --- | --- |
| Go | Toolchain/runtime | Support window follows the active Fiber major version |
| Fiber v3 | HTTP/runtime lifecycle | Fiber hooks own runtime lifecycle |
| Fiber Contrib Uptime | Uptime engine | Upstream semantics remain authoritative unless explicitly overridden |
| Fiber Storage Redis | Optional external persistence | Borrowed connection passed to Uptime's native Redis backend |
| go-redis | Owned Redis connection | ParseURL, context-aware Ping and explicit client Close |
| bbolt | Embedded persistence | Single-process file locking and explicit transaction model |
| YAML v3 | Configuration decoding | Known fields, duplicate rejection, single document; scalar errors must not expose secrets |

The Store contract, unchanged since v0.2.2, distinguishes nil service selections (all registered
services) from explicit empty selections (none); query bounds are inclusive,
retention/rollup bounds are exclusive, and ordering is unspecified. Callers own
initialization and shutdown. `ExpectedSlots` runs only during the rollup call;
our backend additionally guarantees it runs outside database transactions.

P3–P6-A verify these lifecycle assumptions against pinned source and tests:

- Native TLS is tls.NewListener around the tracked raw listener, passed to Fiber
  App.Listener. TLS >=1.2 uses config's preloaded certificate without file reads
  or redundant Fiber ListenConfig TLS fields. Transport tracking remains inside
  TLS so stalled handshakes can be forced closed and drained on shutdown timeout.
- Fiber BasicAuth owns Basic header parsing, NFC credential normalization,
  challenge/cache headers, and password verification via Users. LibreCosta narrows
  configuration to bcrypt ($2a$/$2b$/$2y$) and validates structure plus bcrypt.Cost;
  it never copies Fiber's hash parser or compares passwords in config check.
- Construct optional BasicAuth after public health and before uptime.New; convert
  only constructor panic to a safe error. Auth's Next uses the exact configured
  UI namespace; similar prefixes remain unprotected. No request logger is added.

- `uptime.New` starts runtime workers during construction. Open selected storage,
  complete preflight Ping, and bind first. bbolt supplies `Config.Storage` with
  `Config.Store = nil`; Redis supplies `Config.Store` and `StorageKeyPrefix` with
  `Config.Storage = nil`. Never set self-service fields or implement a Redis Store.
- Redis runtime parses the normalized URL with go-redis, enables
  `ContextTimeoutEnabled`, and constructs a client plus `NewFromConnection`.
  Config validation uses ParseURL offline; it never constructs the client.
- Fiber Storage Redis v3.6.2 borrows that client. Closing its handle does not close
  the connection. After workers and HTTP stop, close the Fiber handle then the
  owned client, joining both failures. Never call Reset or database flush commands.
- LibreCosta's five-second Redis preflight is fatal before bind; keep Uptime's
  subsequent Init/Ping unchanged. Errors after preflight retain upstream's
  degraded/recoverable policy. Readiness directly Pings the client with a
  one-second context. Neither failure path selects a different backend.
- Upstream trims colons around StorageKeyPrefix. The loader rejects such prefixes
  instead, so the normalized namespace is passed unchanged. Redis key format,
  heartbeat and history semantics remain upstream-owned.
- A custom Store is already initialized and caller-owned; upstream does not close
  it. Uptime's Fiber pre-shutdown hook cancels/waits its workers. Close bbolt only
  after Fiber shutdown and HTTP handler completion, including failure paths.
- Register ready=false pre-shutdown before Uptime. Fiber's OnListen occurs before
  fasthttp Serve takes the listener: the private listener's first Accept signals
  that shutdown can safely close it. A shutdown timeout does not itself force-close
  every active connection, so the wrapper closes transport I/O and waits for the
  server to finish those connections before releasing storage.
- Uptime constructor rejection panics rather than returning an error. Convert this
  boundary into a secret-safe operational error without rewriting degraded runtime
  policy. Configuration loading enforces v0.2.2's endpoint timeout >= 1ms rule,
  as well as timeout <= effective endpoint interval, before runtime construction.
- Empty UI description/footer are defaulted again upstream; the P3 config corrective
  change rejects explicit empty values. An empty favicon remains supported.
- Storage operation failures after successful handoff retain upstream degraded
  behavior. Target failures remain monitoring data, independent of process readiness.

- Prefer Fiber-native capabilities when they satisfy product requirements.
- Do not introduce a parallel runtime lifecycle framework.
- Fiber Contrib Uptime is the engine; do not casually fork or copy it.
- Its exported `uptime/storage.Store` contract is the intended integration
  boundary for the public bbolt backend. Do not depend on unexported internals.
- Shared Redis persistence does not imply distributed probe scheduling: each
  application instance still runs its own probes.
- Before material upgrades, inspect upstream release/migration notes and the
  affected source, tests, and contracts. Validate assumptions against the actual
  selected version, rather than treating design-time API sketches as authoritative.
- Follow Fiber's supported Go window; keep `go.mod`, CI, and documentation aligned.

The [product design](../docs/design/v0.1.0-product-technical-design.md) captures
initial intended integrations. Upstream changes do not authorize silently
changing a product contract or broadening the current phase.
