# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added

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

- Reject leading zeros in CalendarDays values, preserving canonical positive `Nd` format.
- Validate endpoint timeout >= 1ms during configuration loading, matching Fiber
  Uptime v0.2.0 so sub-millisecond values cannot pass config check and fail at serve.
- Prevented newly registered instances with zero `LastSeenAt` from expiring during cleanup.
- Removed full sample-day scans from heartbeat and current-day query hot paths,
  retaining full integrity validation during rollup and raw-sample cleanup.
- Reject explicitly empty UI description/footer so Fiber Uptime cannot replace
  them with upstream defaults; omitted values retain DeepFurry defaults.
