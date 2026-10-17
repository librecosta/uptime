# LibreCosta Uptime

A tiny, self-hosted uptime service built around Fiber.

One binary, one YAML file, and zero external infrastructure required by default.

> [!NOTE]
> Uptime is under active development toward v0.1.0. It provides a reusable public
> bbolt backend, strict YAML validation, and a standalone HTTP monitoring runtime.
> Optional Redis storage, native static TLS, and single-user Basic Auth are implemented.

## Overview

LibreCosta Uptime aims to make HTTP service availability and persistent history
easy to operate in a small standalone product. Fiber Contrib Uptime provides
the monitoring engine; the product composes its configuration, storage, and
runtime lifecycle.

## Design Principles

- **Fiber-native:** use the framework and engine's existing capabilities.
- **Zero infrastructure by default:** embedded bbolt persistence.
- **One binary:** keep deployment small.
- **Configuration-driven:** YAML describes the monitored services.
- **Explicit history lifecycle:** configuration changes never silently delete history.
- **Small dependency surface:** add dependencies only when implementation needs them.
- **Predictable operations:** make startup, shutdown, and failure behavior explicit.

## Planned v0.1.0

Planned features include HTTP/HTTPS endpoint monitoring, a status page and JSON
API, a reusable public bbolt backend, optional Redis persistence, optional TLS
and Basic Auth, health endpoints, and explicit service history export/removal.
The public bbolt backend, configuration validation, HTTP runtime, built-in status
page/API, health endpoints, Redis, TLS, and Basic Auth are implemented.
Service listing, Archive v1 export, and guarded service removal are implemented. See the
[product/technical design](docs/design/v0.1.0-product-technical-design.md) for
the intended behavior and boundaries.

## Current Development Status

P1 adds `github.com/librecosta/uptime/storage/bbolt`, directly implementing Fiber
Contrib Uptime's public `storage.Store` contract using bbolt v1.5.0. It
provides schema-versioned persistence, atomic service-level heartbeat deduplication,
rollup, queries, cleanup, explicit service removal, and contract tests.

P2 adds normalized configuration and `config check`. No-argument invocation shows
help; help/version remain available. Unknown commands or unexpected arguments
return usage errors. Configuration uses stable YAML v3 with no CLI/config framework.

P3 adds `serve`, composing Fiber v3 and Fiber Uptime with the public
bbolt Store. The runtime monitors only configured endpoints, with no self service.
P4 adds optional Redis persistence through Fiber Storage Redis and Uptime's native
Redis backend, retaining bbolt as the zero-infrastructure default.
P5 adds native static TLS and Fiber BasicAuth for the dashboard/API namespace.
P5.5 completes LibreCosta branding, defaults to loopback access, redirects the
root to the configured dashboard, and upgrades Fiber Uptime to v0.2.2.
P6-A adds internal standalone storage access for history queries and explicit
service removal, using upstream PR #2535's merged Redis Store API. The dependency
is temporarily pinned to commit `e811012f822675810017efdf00823849f97f2323` via a
Go pseudo-version; see the [upstream contract](contracts/upstream.md).
P6-B1 adds `service list` and `service export` using that query boundary, with
maintenance configuration and safe Archive v1 JSON output. P6-B2 adds explicit
removal of persisted services absent from the current YAML endpoint set.
P7-A adds cross-platform distribution packaging and a manual Actions preview
workflow. These are development previews, not official releases.
P7-B adds non-root Linux Docker images and a hardened systemd deployment example.

## Build / Distribution Preview

From the repository root with Go 1.26 or a supported newer Go:

```sh
go run ./tools/dist --version dev --commit "$(git rev-parse HEAD)" --output dist
```

PowerShell:

```powershell
$sha = git rev-parse HEAD
go run ./tools/dist --version dev --commit $sha --output dist
```

The standard-library tool builds with CGO disabled, injects the supplied version
and full 40-digit Git SHA, and verifies each archive before publishing `dist/`.
An existing nonempty output directory is refused. `make build` remains a compile
check; ordinary `go run ./cmd/uptime version` still reports `dev` / `unknown`.

| Platform | Preview archive |
| --- | --- |
| Linux amd64 | `librecosta-uptime_dev_linux_amd64.tar.gz` |
| Linux arm64 | `librecosta-uptime_dev_linux_arm64.tar.gz` |
| macOS amd64 | `librecosta-uptime_dev_darwin_amd64.tar.gz` |
| macOS arm64 | `librecosta-uptime_dev_darwin_arm64.tar.gz` |
| Windows amd64 | `librecosta-uptime_dev_windows_amd64.zip` |

`SHA256SUMS` covers all five archives in filename order. With Go available, verify
the complete set on any supported host (this does not execute its binaries):

```sh
go run ./tools/dist --verify --version dev --output dist
```

Alternatively, in `dist/`, use `sha256sum --check SHA256SUMS` on Linux or
`shasum -a 256 --check SHA256SUMS` on macOS. On PowerShell, compare
`(Get-FileHash ./dist/librecosta-uptime_dev_windows_amd64.zip -Algorithm SHA256).Hash`
with that archive's entry in `SHA256SUMS`.

Extract the archive for your platform into an empty directory. It contains only
`uptime` (`uptime.exe` on Windows), `uptime.example.yaml`, `README.md`, and `LICENSE`.
In that directory, copy `uptime.example.yaml` to `uptime.yaml`, edit your endpoints,
then run `./uptime version`, `./uptime config check --config ./uptime.yaml`, and
`./uptime serve --config ./uptime.yaml` (PowerShell: `./uptime.exe`). No Go installation
is needed to run the binary. Configuration is never generated automatically;
relative configuration, TLS and `./data/uptime.db` paths use the **process working
directory**, not the executable's location. The default listener is
`127.0.0.1:18580`.

The manual [distribution workflow](.github/workflows/distribution.yml) builds with
Go 1.26.x, checks all targets and runs a Linux amd64 CLI smoke test. It uploads only
a seven-day Actions Artifact, without a Tag or Release. GitHub normally exposes
the manual trigger once the workflow reaches the default branch; it does not
change the `develop` / `pre-master` / `master` promotion process.

## Deploy on Linux

- [Docker](docs/deployment/docker.md): build local amd64/arm64 previews, mount
  container-specific configuration read-only and persist bbolt in a named volume.
  The image runs as UID/GID 65532 with system CA roots and no shell. Publish to
  host loopback by default; the container YAML explicitly listens on `:18580`.
- [systemd](docs/deployment/systemd.md): install a verified binary with a dedicated
  service account, fixed working directory, private state and graceful shutdown.

Both guides cover health checks, secure exposure, offline bbolt maintenance,
backup and upgrades. GHCR images and official Releases are not published yet.
The [Docker preview workflow](.github/workflows/docker-preview.yml) builds both
architectures and tests amd64 persistence without pushing images.

## Validate Configuration

The [official example](configs/uptime.example.yaml) validates without environment
variables, a bbolt file, Redis, or TLS certificates:

```sh
go run ./cmd/uptime config check --config configs/uptime.example.yaml
```

Success prints only `configuration is valid`. With no `--config`, the command
reads `./uptime.yaml`. The flag belongs to `config check`, not the root command.
Validation errors go to stderr and return 1; CLI usage errors return 2.

Configuration uses one strict YAML document: unknown fields, duplicate keys,
wrong types, explicit nulls, and merge keys are rejected. Defaults apply only
to omitted fields. `${VAR_NAME}` substitution is one-pass and limited to allowed
active string fields. Inactive TLS/Auth/storage values are neither resolved nor
semantically validated; their YAML structure still has to be valid.

Config check validates matching TLS certificate/key files only when TLS is enabled.
Relative paths use the process working directory. It does not create directories,
open storage, connect Redis, resolve DNS, probe URLs, or bind ports. Auth validation
requires one NFC-normalized username without colon/control characters and a valid
bcrypt hash (`$2a$`, `$2b$`, or `$2y$`). Hash validation checks structure and cost
without comparing a password. Disabled auth values remain ignored.
No configuration or secret values are dumped. Avoid placing secrets in URLs.

See the [configuration and CLI contract](contracts/compatibility.md) for defaults,
field rules, and normalization. Description/footer may be omitted for LibreCosta
defaults but cannot be explicitly empty; an empty favicon URL remains valid.

## Run the Service

Create `./uptime.yaml` using the official example and set your endpoints, then run:

```sh
uptime config check --config ./uptime.yaml
uptime serve --config ./uptime.yaml
```

From source, use `go run ./cmd/uptime serve --config ./uptime.yaml`.
`serve` defaults to `./uptime.yaml`; `serve --help` documents its only configuration
flag. Relative file paths use the process working directory. The default listener
is `127.0.0.1:18580`, and bbolt uses `./data/uptime.db` with exclusive file ownership.
Open **http://127.0.0.1:18580/** locally; it redirects to the configured `ui.path`
(default `/uptime`). Successful startup logs the product, bound address, HTTP/HTTPS,
and dashboard entry. HTTPS entries require a hostname or IP covered by your certificate.

For public, Docker, or reverse proxy deployments needing access from other hosts,
explicitly set `server.address: ":18580"` and publish/forward that port as appropriate.
A proxy on the same host can use the default loopback listener. Existing explicit
addresses, including `:8080`, retain their behavior. See
[ADR 0001](docs/decisions/0001-local-first-startup.md) for the default change.

| Route | Behavior |
| --- | --- |
| GET/HEAD `/` | 302 to the configured `ui.path`, without forwarding query parameters |
| `/livez` | 200 `ok`, without checking storage or targets |
| `/readyz` | 200 `ready` after listen with a successful one-second-context storage Ping; otherwise 503 `not ready` |
| `/uptime` | Built-in Fiber Uptime dashboard (configurable `ui.path`) |
| `/uptime/api/status` | Built-in status API under the same UI path |

Health bodies end in a newline and use `text/plain; charset=utf-8` and
`Cache-Control: no-store`. A DOWN target does not make the monitor unready.
The root redirect is public; following it still requires dashboard authentication
when enabled. Other methods on `/` and unmatched routes remain 404.

Serving supports **bbolt or Redis, HTTP or HTTPS, and optional Basic Auth**.
Startup storage/schema/lock, preflight Ping, or bind failures are fatal. Later storage operation
failures retain upstream degraded behavior and do not automatically stop the process.

Ctrl+C or supported SIGTERM requests graceful shutdown: readiness becomes false,
Fiber stops Uptime workers and HTTP, then the application closes selected storage. Shutdown
timeout forces remaining connection I/O closed and waits for handlers before
closing storage; blocked storage I/O cannot be forcibly canceled. Normal shutdown
returns 0, operational/cleanup failures 1, and CLI usage errors 2.

## Native TLS and Basic Auth

Add these sections to your configuration alongside storage and endpoints:

```yaml
server:
  address: ":8443"
  tls:
    enabled: true
    cert_file: "./tls/fullchain.pem"
    key_file: "./tls/privkey.pem"
auth:
  enabled: true
  basic:
    username: "${UPTIME_AUTH_USERNAME}"
    password_hash: "${UPTIME_AUTH_PASSWORD_HASH}"
```

TLS uses a static matching cert/key loaded during configuration validation, with
TLS 1.2 as the minimum. Run uses that loaded keypair without reading the files
again. Enabling TLS makes the listener HTTPS-only; invalid TLS never falls back
to HTTP. Certificate replacement requires a restart. ACME, mTLS, hot reload and
custom TLS tuning are not supported.

Auth is optional and independent of TLS: HTTP/public, HTTP/Auth, HTTPS/public,
and HTTPS/Auth are supported. It protects the exact `ui.path` and slash descendants
including the status API. For `/status`, `/status2` and `/status-page` are outside
the protected namespace. `/livez` and `/readyz` always remain public. The realm is
fixed to `LibreCosta Uptime`; there is one account and no login page or session.

Set `UPTIME_AUTH_PASSWORD_HASH` to a bcrypt hash, not a plaintext password or SHA
verifier. Unicode usernames are allowed and normalized to NFC. For non-ASCII
passwords, generate the bcrypt hash from the NFC-normalized password to match
Fiber's Basic Auth credential normalization. There is no password-generation CLI.

**Basic Auth does not encrypt credentials.** Direct public HTTP exposure is unsafe;
use native TLS or a trusted TLS-terminating reverse proxy. HTTP+Auth remains
available for deployments behind such a proxy.

## Optional Redis Persistence

Replace the storage section in your YAML; keep the remaining endpoint/UI settings:

```yaml
storage:
  type: redis
  redis:
    url: "${UPTIME_REDIS_URL}"
    key_prefix: "fiber:uptime"
```

The URL must use `redis://` or `rediss://`, include a host, have no fragment,
and pass go-redis URL parsing. Credentials, database paths, and supported query
parameters are allowed. Config check parses offline and never connects. `rediss://`
secures the Redis connection independently of native server TLS. The prefix must
be non-empty, without leading/trailing colon or whitespace or any control character;
internal colons such as `prod:asia:uptime` are supported. Rules apply after env expansion.
Redis `uptime.retention` must not exceed `106750d` so its expiry backstop cannot
overflow; config check enforces this offline. This limit does not apply to bbolt.

Startup Pings Redis with a five-second timeout before binding HTTP. A canceled
startup context cleans up normally; connection failure is fatal with a safe error.
Uptime retains its own initialization Ping and degraded recovery. During a Redis
outage `/livez` stays 200, `/readyz` becomes 503, and the process stays running.
Readiness recovers when its one-second Ping succeeds. Shutdown closes the borrowed
Fiber Redis handle and then the owned go-redis client after workers/HTTP finish.

Switching bbolt and Redis selects another persistence source; it does not copy,
merge, dual-write, migrate, delete old history, or fall back on failure. Shared
Redis persistence does not provide leader election or distributed scheduling:
each application instance runs its own probes.

## List and Export Service History

```sh
uptime service list --config ./uptime.yaml
uptime service export api-v1 --config ./uptime.yaml --output ./api-v1.json
```

Both commands default to `./uptime.yaml`; relative paths use the working directory.
Stop `serve` before opening bbolt for maintenance. Its database must already exist;
a missing path fails without creating an empty database. Redis permits online
queries, but separate queries do not form a transactionally consistent snapshot.
Neither command starts monitoring or changes source history.

The list contains persisted IDs in lexical order, `active`/`detached`, and last-seen
UTC time (`-` when unknown). Active means the ID is in the loaded YAML endpoints;
it does not mean online. Detached does not prove that other writers have stopped.
Maintenance commands allow explicit `endpoints: []` for a configuration with no
active services. Omitting `endpoints` is still invalid; `serve` and `config check`
continue to require at least one endpoint. All other validation, including active
TLS/Auth, remains in effect.

Export accepts active or detached IDs and writes Archive v1 JSON with stored service
metadata, all retained daily rows (including history beyond the UI window), and
the current day's raw up-slot count. UTC timestamps and the configured timezone
are recorded. The current-day expected count is theoretical completed slots,
accounts for DST, and does not establish that a detached service was monitored.
Daily rows and current samples are separate sources and should not be added together.
No request headers, URLs, credentials or instance details are included. This is a
history export, not a database backup or import format.

Without `--output`, the file is `uptime-service-YYYYMMDDTHHMMSSZ.json` in the working
directory. Existing files and symlinks are never replaced. The output directory
must exist and support hard links: a complete, synced temporary file is published
atomically, or the operation fails safely. New files use mode 0600 where supported;
Windows access follows the directory's ACL. See the [Archive v1 contract](contracts/compatibility.md#service-list-and-archive-v1-p6-b1)
for the stable fields and time rules. Help uses no configuration or storage;
success returns 0, usage errors 2, and operational failures 1. There is no overwrite option.

## Remove a Retired Service

First remove the ID from YAML and stop or restart every monitor still using the
old configuration. Then run:

```sh
uptime service remove api-v1 --config ./uptime.yaml
```

This permanently deletes the stored service and its history. An ID still in the
loaded YAML is refused before storage is opened; an unknown stored ID is an error.
The command defaults to `./uptime.yaml`, uses the same maintenance validation as
list/export, and accepts explicit `endpoints: []` to remove the final retired service.
There is no `--force`, confirmation prompt, automatic export, or Archive import.

bbolt requires stopping `serve` and exclusive access to the existing database.
Redis removal uses only the configured database and key prefix. Detached status
does not exclude other writers: they may recreate records even after a successful
read-back. Coordinate all writers before removing shared Redis data.

Success prints `service removed` only after read-back and storage Close succeed.
Failures return 1 without success output and never retry deletion automatically.
A deletion attempt may already have changed data even if the operation, read-back,
Close, or output subsequently fails; a failure is not a rollback guarantee.

## Public bbolt Package

Use `Open(Config{Path: "./data/uptime.db"})` from the public package to obtain a
ready `*Store`. `Path` is required; a zero lock timeout defaults to five seconds.
Set `Config.MustExist` for access that must never create a database or parent directory.
Callers own the Store and call `Close` after all users have stopped. The backend
also provides `Name`, `Ping`, and atomic `RemoveService`.

One Store owns a database file at a time. Existing invalid files are rejected,
never adopted or repaired. There is no raw DB/bucket API and no active/detached
product policy in the backend. See the [storage design](docs/design/bbolt-storage.md)
for schema, cancellation limitations, and cold-backup guidance.

## Architecture

`cmd/uptime` calls `internal/config` for validation and `internal/app` for serving.
`storage/bbolt` is independently reusable and imports no project `internal/*`
package. `internal/app.New` checks capabilities without I/O; its single-use `Run`
owns listener, Fiber/Uptime, and storage lifetime. `internal/service` derives list
status and guards explicit removal; `internal/archive` builds and safely publishes
Archive v1 from Store queries.
`internal/app.OpenServiceStorage` separately opens bbolt or Redis for history
queries, explicit removal, and Close, without HTTP or monitoring workers. bbolt
requires offline access; Redis deletion must be coordinated with active writers.
See the [architecture guide](.agents/architecture.md).

## Development

Use Go 1.26 or a supported newer version and GNU Make. CI is configured to run
the same checks on Go 1.26.x and 1.27.x.

```sh
make check
make race
```

`make check` checks formatting, runs `go vet` and tests, and verifies package
builds without producing a packaged binary. `make race` separately checks the
storage, runtime, archive and CLI packages with the Go race detector and runs in a dedicated Go 1.27.x
Ubuntu CI job. It requires a supported platform and C compiler. Use `make fmt`
to format repository Go source, excluding ignored caches. The
[playbook](.agents/playbook.md) documents individual checks and equivalent Go
commands when Make is unavailable.

Real Redis tests are opt-in and otherwise skip, so normal checks need no Redis:

```sh
UPTIME_TEST_REDIS_URL=redis://127.0.0.1:6379/15 go test ./internal/app/... -run '^TestRedisIntegration' -count=1
```

Use a disposable test Redis. Tests use unique prefixes and clean only their own
keys, never reset/flush the database. A separate Go 1.27.x CI job runs these tests
against official `redis:8.2.1-alpine`; the race job does not start Redis.

Try the implemented CLI:

```sh
go run ./cmd/uptime --help
go run ./cmd/uptime version
go run ./cmd/uptime config check --help
go run ./cmd/uptime serve --help
go run ./cmd/uptime service --help
```

Development version output identifies `dev`, the Go runtime version, and commit
`unknown`. The package-level version and commit strings allow future linker
injection; no release packaging workflow exists.

## Documentation

- [v0.1.0 Product & Technical Design](docs/design/v0.1.0-product-technical-design.md)
- [bbolt Storage Design](docs/design/bbolt-storage.md)
- [Agent Entry Point](AGENTS.md) and [Agent Architecture](.agents/architecture.md)
- [Engineering Contracts](contracts/): [Compatibility](contracts/compatibility.md),
  [Persistence](contracts/persistence.md), [Upstream](contracts/upstream.md)
- [Change Playbook](.agents/playbook.md)
- [Architecture Decisions](docs/decisions/README.md)
- [Changelog](CHANGELOG.md)
- [LibreCosta community files](https://github.com/librecosta/.github)

## License

[MIT](LICENSE), Copyright (c) 2026 DeepFurry.
