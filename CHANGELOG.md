# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

- P7-B: multi-stage Linux amd64/arm64 Docker previews using Go 1.26 and Distroless
  nonroot with CA roots, an isolated build context, container YAML and persistent
  data ownership; no product defaults or dependencies change.
- Restricted systemd unit and Docker/systemd deployment, shutdown, upgrade and
  offline backup guides. Docker preview CI validates architecture, non-root
  startup, health, graceful stop and retained bbolt data without pushing images.

- P7-A: standard-library cross-platform builds for Linux/macOS amd64/arm64 and
  Windows amd64, explicit version/full Git SHA, allowlisted tar.gz/zip packages,
  SHA256SUMS verification and staged publication without overwriting existing data.
- Manual Go 1.26.x Actions distribution previews with binary/architecture checks,
  Linux CLI smoke and seven-day artifacts; no Tag or Release publication.

- P6-B2: `service remove <id>` refuses configured IDs before opening storage,
  requires persisted history, deletes through the standalone session, and reads
  back/closes before success output. Includes last-service retirement, offline
  locking, Redis prefix isolation, and safe failure regression coverage.

- P6-B1: `service list` with YAML-derived active/detached status and UTC times,
  and `service export` with versioned Archive v1 JSON, complete persisted daily
  history, DST-aware current-day slots, and atomic no-overwrite file publication.
- Maintenance configuration accepts explicit `endpoints: []` while serve/config
  check retain their original validation; all other strict/active rules still apply.
- CLI/archive regression tests and shared bbolt/Redis list/export coverage.

- P6-A: internal standalone bbolt/Redis history queries, explicit service removal,
  and caller-owned cleanup, without a new CLI, archive format, or monitoring runtime.
- Standalone storage coverage for locking, history, deletion, cancellation, safe
  errors, resource ownership, and opt-in real Redis prefix/retention behavior.

- P5.5: GET/HEAD `/` redirects with HTTP 302 to the configured UI path without
  forwarding queries; startup logs show the listener, protocol, and dashboard entry.
- Original bbolt Schema v1 fixture and redirect/authentication regression coverage.

- Native static TLS using the preloaded certificate, TLS 1.2 minimum, and tracked
  connection draining, including stalled-handshake shutdown regression tests.
- Optional Fiber BasicAuth for the UI namespace with public health routes, one
  NFC-normalized username, bcrypt-only hashes, and HTTP/HTTPS combination tests.
- Optional Redis runtime using Fiber Storage Redis and Uptime's native backend,
  with startup preflight, recoverable readiness, and explicit client ownership.
- Opt-in real Redis integration tests and a separate Go 1.27.x Redis CI job.
- Initial repository and engineering foundation.
- Public bbolt storage backend implementing Fiber Uptime v0.2.0's Store contract.
- Schema v1 with database identity, strict decoding, transactional persistence,
  and behavioral tests for concurrency, corruption, cleanup, locking, and reopen.
- Separate `make race` verification and Go 1.27.x Ubuntu race CI job.
- Strict YAML v3 configuration with omission-aware defaults, active one-pass
  environment interpolation, typed normalization, and secret-safe validation.
- `uptime config check`, an offline-validating official example, and configuration/
  CLI regression tests, including generated TLS keypairs.
- `uptime serve` with bbolt, Fiber v3.5.0, Fiber Uptime v0.2.0 endpoint probing,
  built-in dashboard/API, liveness/readiness routes, and graceful signal shutdown.
- Single-use application lifecycle, explicit unsupported capability gates, and
  integration tests for UP/DOWN targets, persistence, cancellation, and failures.

### Changed

- Pin Fiber Uptime to merged PR #2535 commit `e811012f822675810017efdf00823849f97f2323`
  and its necessary dependency versions; see the upstream contract.
- Reject Redis retention above 106750 days during offline config validation,
  matching upstream's TTL bound without limiting bbolt retention.

- P5.5: omitted `server.address` now defaults to `127.0.0.1:18580`; explicitly
  configure `:18580` for access beyond loopback. Explicit addresses keep their behavior.
- Complete user-facing LibreCosta Uptime branding and rename the unimplemented
  Service Archive plan, preserving the original MIT attribution, bbolt format,
  Schema v1, and Redis `fiber:uptime` prefix.
- Upgrade Fiber Uptime from v0.2.0 to v0.2.2 and only its required module versions;
  see the upstream contract for the dependency and behavior review.

- Move the Go module to `github.com/librecosta/uptime`; external users must
  update module requirements and imports from the former DeepFurry path.

- Active auth now rejects non-bcrypt verifiers and usernames containing colons or
  Unicode controls; pinned x/crypto and x/text dependencies are now direct.
- Validate active Redis URLs with pinned go-redis ParseURL and reject fragments,
  edge colons/whitespace and control characters in Redis key prefixes, offline
  and without exposing secrets. Redis dependencies are now direct, without upgrades.
- Updated checkout/setup-go GitHub Actions to v7.
- Limited formatting to repository Go files so ignored dependency caches remain untouched.
- Extended the existing race target/job to include `internal/app`.

### Fixed

- Standalone bbolt management requires an existing database, enforced during file
  open by `Config.MustExist`, without changing serve's first-run creation behavior.

- Reject leading zeros in CalendarDays values, preserving canonical positive `Nd` format.
- Validate endpoint timeout >= 1ms during configuration loading, matching Fiber
  Uptime v0.2.0 so sub-millisecond values cannot pass config check and fail at serve.
- Prevented newly registered instances with zero `LastSeenAt` from expiring during cleanup.
- Removed full sample-day scans from heartbeat and current-day query hot paths,
  retaining full integrity validation during rollup and raw-sample cleanup.
- Reject explicitly empty UI description/footer so Fiber Uptime cannot replace
  them with upstream defaults; omitted values retain DeepFurry defaults.
