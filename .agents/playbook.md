# Change Playbook

Repository context is authoritative; Agent Skills are optional accelerators and
must not be required to understand or safely modify the repository.

## Workflow

1. Understand the requested scope and acceptance criteria. Identify conflicts
   with current implementation or design before changing either.
2. Inspect `git status`, the current branch, `AGENTS.md`, architecture, and the
   relevant contracts. Preserve existing user changes; do not overwrite them.
3. Read affected source/tests and establish a verification baseline. Work only
   within the repository and avoid assumptions about neighboring directories.
4. Make the smallest coherent change. Avoid unrelated refactors, dependency
   upgrades, speculative abstractions, and empty packages.
5. Add behavioral tests for observable changes, including error/exit behavior.
6. Run verification, inspect the complete final diff (including new files), and
   check for unintended artifacts, dependencies, or scope expansion.
7. Update the changelog for notable changes. Add an ADR only when an important
   architectural decision is not already captured by the initial baseline.
8. Report the result and exact validation performed, including failures and
   unavailable checks. Push or publish only when explicitly requested.

## Task Routing

| Change | Read first |
| --- | --- |
| CLI, configuration, user-visible behavior | [Compatibility](../contracts/compatibility.md) |
| bbolt, history, retention, schema | [Persistence](../contracts/persistence.md) |
| Fiber, Fiber Uptime, Redis, bbolt behavior or upgrades | [Upstream](../contracts/upstream.md) |
| Architectural boundaries | [Architecture](architecture.md), relevant [design](../docs/design/), then consider an [ADR](../docs/decisions/README.md) |

## Verification

From the repository root, canonical verification is:

```sh
make check
```

It checks formatting, vets, tests, and builds packages. `make build` verifies
compilation without packaging a binary. `make fmt` applies Go formatting;
`make fmt-check`, `make vet`, `make test`, and `make build` run individual checks.
GNU Make and Go 1.26 or a supported newer Go are required. CI verifies Go 1.26.x
and 1.27.x using the same `make check` entry point.

For storage/runtime changes also run `make race`
(`go test -race ./storage/bbolt/... ./internal/app/... ./internal/archive/... ./cmd/uptime/...`).
It is separate from `make check` and has a dedicated Go 1.27.x Ubuntu CI job.
The race detector requires a supported platform and C compiler; report local
limitations rather than claiming an unavailable run passed.

If GNU Make is unavailable, equivalent Go commands are:

```sh
go fmt ./...
go vet ./...
go test ./...
go build -o /dev/null ./...
```

On Windows, use `go build -o NUL ./...` for the final command. Explicitly using
the host's null device prevents a single-main-package build from leaving an
executable in the working tree; Make selects the device automatically.

For final verification, formatting must leave no additional diff. Make applies
`gofmt` to Git-listed and unignored Go files, including new source, so local
dependency caches are not formatted. `go fmt ./...` is the package-scoped
fallback. Both formatting commands modify source; inspect their changes before
declaring the final tree verified.

When module inputs change, run `go mod tidy`, inspect `go.mod`/`go.sum`, and run
`go list -m all`. Direct dependencies are Fiber v3, Fiber Contrib Uptime,
Fiber Storage Redis, go-redis, bbolt, YAML v3, x/crypto and x/text;
their transitive module graph is expected. Always finish with:

```sh
git diff --check
git status --short
git diff
```

Review staged changes with `git diff --cached` when present and inspect untracked
files as well: ordinary `git diff` does not include them. CLI smoke checks are:

```sh
go run ./cmd/uptime --help
go run ./cmd/uptime version
go run ./cmd/uptime config check --config configs/uptime.example.yaml
go run ./cmd/uptime config check --help
go run ./cmd/uptime serve --help
go run ./cmd/uptime service --help
```

Do not claim unavailable toolchain or remote CI checks passed locally. P1 tests
the public backend using real temporary database files. P2 config tests inject
environment lookup and generate temporary TLS keypairs in Go. The official example
must validate without secrets or external services; config check never starts runtime.
P3 integration tests use local `httptest.Server` targets, ephemeral listeners and
temporary bbolt, with deadline polling and context cancellation. Verify Linux and
Windows amd64 builds after lifecycle/signal changes; no release packaging is implied.

P4 real Redis tests require an explicitly supplied `UPTIME_TEST_REDIS_URL`:

```sh
UPTIME_TEST_REDIS_URL=redis://127.0.0.1:6379/15 go test ./internal/app/... -run '^TestRedisIntegration' -count=1
```

Without it, these tests skip; `make check` and `make race` require no Redis.
The separate Go 1.27.x `redis-integration` CI job uses disposable official
`redis:8.2.1-alpine` with a health check and runs only that test group. Real Redis
is not part of the race job. Each test owns a unique prefix and deletes only its
own keys; never use Reset, FLUSHDB, or FLUSHALL. Tests start from YAML, use real
Redis with controlled client fault injection, and cover probing, history, isolation,
startup failure, degraded operation, recovery, and shutdown ownership. Report skips
separately from actual Redis integration passes.
P6-A standalone storage tests use the same Redis group/job and random-prefix
cleanup. They cover history/removal, retention TTL, isolation and resource
ownership; regular checks also cover offline bbolt access/locking and safe errors.
P6-B1 extends that same group with list/export checks through both standalone
backends. Archive/CLI tests cover UTC/DST, full history, failure cleanup and
no-replace publication. Verify file publication on Windows and Linux; report a
platform's unavailable symlink or hard-link support accurately. No extra Redis job
or container test dependency is needed.
P6-B2 adds CLI checks for refusal before storage open, detached/last-service removal,
unknown IDs, offline locks, and failure/Close ordering. The same Redis group also
verifies guarded removal and preservation of other prefixes and services.

P5 tests generate a CA and SAN certificate in Go and use clients that trust that
CA. Never bypass verification with InsecureSkipVerify. Cover all four HTTP/HTTPS
and public/Auth combinations, UI namespace boundaries, safe constructor rejection,
and stalled-handshake shutdown with storage closing after worker/connection drain.
These tests stay in normal checks and the existing runtime race scope.

## Distribution Preview (P7-A)

Run `make check` once, then from the repository root:

```sh
go run ./tools/dist --version dev --commit "$(git rev-parse HEAD)" --output dist
```

PowerShell equivalent:

```powershell
$sha = git rev-parse HEAD
go run ./tools/dist --version dev --commit $sha --output dist
```

The output parent must exist; output must be absent or empty. Choose a new output
path for another build rather than deleting existing files automatically. The tool
builds only `./cmd/uptime` with readonly modules, CGO disabled, `-trimpath`,
`-buildvcs=false`, and explicit linker metadata. It verifies five archives,
checksums, permissions, Go build metadata and ELF/Mach-O/PE architectures before
renaming its staging directory into place. Temporary workspace cleanup is attempted
on failures/cancellation; force-killing a process may leave a `.uptime-dist-*`
directory, never a complete final output. Do not mistake staging files for artifacts.

```sh
go run ./tools/dist --verify --version dev --output dist
```

Verification inspects packages without running binaries. Separately extract the
host archive into a temporary directory, run `version` (match the supplied version
and full SHA), `--help`, and `config check --config uptime.example.yaml` there.
Each archive must contain the binary, example YAML, README, LICENSE, and unchanged
`docs/assets/uptime-dashboard.webp`; check README's relative image after extraction.
Config check must not create `data/`. Go omits linker flags from build metadata
with `-trimpath`, so native execution is the version-injection acceptance check.
The preview workflow performs these steps on Linux amd64 with Go 1.26.x and uploads
only a seven-day Actions Artifact. Report cross-compiled targets separately from
native executions and remote CI that has not run. No extra race/Redis run is
required for packaging-only changes; preserve the existing CI jobs.

## Docker / systemd Deployment (P7-B)

Run `make check` once for repository validation. With a working Linux Docker
daemon and Buildx, build from the repository root (PowerShell: assign
`$sha = git rev-parse HEAD` and use `--build-arg "COMMIT=$sha"`):

```sh
docker buildx build --platform linux/amd64 --load --build-arg VERSION=dev --build-arg COMMIT="$(git rev-parse HEAD)" --tag librecosta-uptime:dev-amd64 .
docker buildx build --platform linux/arm64 --load --build-arg VERSION=dev --build-arg COMMIT="$(git rev-parse HEAD)" --tag librecosta-uptime:dev-arm64 .
docker image inspect --format '{{.Os}}/{{.Architecture}} {{.Config.User}}' librecosta-uptime:dev-amd64 librecosta-uptime:dev-arm64
bash deploy/docker/smoke.sh librecosta-uptime:dev-amd64 "$(git rev-parse HEAD)"
```

Smoke requires Bash, curl, tar and an amd64 Linux Docker host. It checks version,
help/config, CA roots, runtime contents, missing config, non-root named-volume
initialization, health/root redirect, graceful stop, detached data after container
replacement, and read-only storage failure. It traps exit/signals to remove only
its unique containers, volume and temporary files. Verify cleanup if interrupted;
never prune unrelated Docker resources. No ARM64 execution or QEMU is required.

The Docker preview workflow runs this same smoke, manually or after a develop push
touching Docker inputs. It never logs in or pushes to a registry. Keep existing CI
and P7-A unchanged. Do not repeat race/Redis for deployment-only changes. If Docker
is unavailable, report image/run checks unverified, not passed. A newly added
workflow has not run remotely until a permitted push and Actions run occur.

On a systemd host, follow the installation guide and run `systemd-analyze verify`
after installing the binary/unit. Report static review separately from an actual
service start; do not install or change host services merely to make a local check
pass. Deployment examples must preserve process-CWD paths and offline bbolt locks.
