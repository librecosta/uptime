# Upstream Contract

P5.5 directly uses Fiber v3.5.0, Fiber Contrib Uptime v0.2.2, Fiber Storage Redis
v3.6.1, go-redis v9.23.0, bbolt v1.5.0, and stable `go.yaml.in/yaml/v3` v3.0.5.
`go.mod` is authoritative for versions. P4 promoted Redis dependencies from
transitive to direct; P5.5 updates the versions required by Uptime. YAML v3 replaces
the original provisional v4 choice; no CLI/config framework is used.
P5 also directly uses pinned x/crypto v0.57.0 for bcrypt structural/cost validation
and x/text v0.42.0 for username NFC normalization; these versions remain unchanged in P5.5.

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

The v0.2.2 Store contract distinguishes nil service selections (all registered
services) from explicit empty selections (none); query bounds are inclusive,
retention/rollup bounds are exclusive, and ordering is unspecified. Callers own
initialization and shutdown. `ExpectedSlots` runs only during the rollup call;
our backend additionally guarantees it runs outside database transactions.

P3–P5.5 verify these lifecycle assumptions against pinned source and tests:

- Native TLS is tls.NewListener around the tracked raw listener, passed to Fiber
  App.Listener. TLS >=1.2 uses config's preloaded certificate without file reads
  or redundant Fiber ListenConfig TLS fields. Transport tracking remains inside
  TLS so stalled handshakes can be forced closed and drained on shutdown timeout.
- Fiber v3.5.0 BasicAuth owns Basic header parsing, NFC credential normalization,
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
- Fiber Storage Redis v3.6.1 borrows that client. Closing its handle does not close
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
