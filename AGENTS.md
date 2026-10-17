# Agent Guide

## Repository Purpose

LibreCosta Uptime is a small, self-hosted uptime product built around Fiber.
P1 provides a public bbolt backend; P2 adds strict configuration loading;
P3 composes Fiber/Uptime into `serve` with bbolt, plain HTTP, health endpoints,
and graceful shutdown. P4 adds upstream Redis persistence. P5 adds static native
TLS and optional single-user Basic Auth using Fiber's official middleware.
P5.5 adds loopback defaults, a configured root redirect, LibreCosta branding, and
Fiber Uptime v0.2.2; persistent identities and schema remain unchanged.
P6-A adds internal standalone history access and explicit removal using the
merged upstream Redis Store API. Service CLI/archive behavior remains unimplemented.

## Start Here

1. Read [.agents/architecture.md](.agents/architecture.md).
2. Read [.agents/playbook.md](.agents/playbook.md).
3. Read the relevant [contracts](contracts/).
4. Inspect current source and tests.
5. Consult [designs](docs/design/) and [ADRs](docs/decisions/) for architectural intent.

## Repository Map

- `cmd/uptime`: executable entry point, minimal CLI, behavioral tests.
- `internal/config`: raw YAML, active env resolution, normalized types, validation.
- `internal/app`: Fiber/Uptime composition, health/lifecycle, standalone storage access.
- `configs`: official example that validates without external services or secrets.
- `storage/bbolt`: public Fiber Uptime Store implementation and persistence tests.
- `.agents`: architectural boundaries and change workflow.
- `contracts`: compatibility, persistence, and upstream constraints.
- `docs/design`: planned v0.1.0 baseline and bbolt design rationale.
- `docs/decisions`: significant decisions after the baseline.
- `Makefile`, `.github/workflows/ci.yml`: local verification and CI.

## Sources of Truth

Source/tests define implemented behavior; `go.mod` and CI define toolchain policy.
Contracts define stable constraints. The product design is the future v0.1.0
baseline, not evidence that a feature exists. Schemas and migrations become the
authority for concrete persistent representations when implemented. Surface any
conflict before changing behavior; do not silently rewrite a contract or design.

## Core Rules

- Work within this repository; do not depend on surrounding workspace content.
- Stay Fiber-native, with a deliberately small dependency surface. Direct dependencies
  are Fiber v3, Fiber Contrib Uptime, Fiber Storage Redis, go-redis, bbolt, and
  stable YAML v3, x/crypto and x/text; keep their pinned versions.
- Do not reimplement Fiber/Fiber Contrib Uptime behavior without a concrete product reason.
- YAML is the user-facing configuration source of truth. Resolve and
  validate active configuration only; strict decoding still rejects unknown keys.
- `internal/config.LoadFile` returns fully normalized active branches. Config check
  reads only YAML/active TLS files; it never opens storage or starts network/runtime work.
- Never log or dump normalized configuration or include configured secret values in errors.
- Active Basic Auth is bcrypt-only with an NFC-normalized username, without
  colon/control characters. Fiber owns credential parsing and verification;
  health stays public and only the exact UI path and descendants require auth.
- TLS wraps the tracked raw listener using the preloaded certificate and TLS 1.2
  minimum. Never reread certificates in Run, downgrade to HTTP, or bypass tracking.
- `internal/app.New` performs no I/O. Single-use `Run` opens the selected storage,
  completes preflight Ping (Redis: five seconds), and binds before
  constructing Uptime, then serves only configured endpoints without a self service.
- The public `storage/bbolt` package must remain independent of `internal/*`.
- Never delete service history implicitly. Version persistent schema evolution.
- Fiber hooks own runtime lifecycle once the application is running. Storage
  must outlive Uptime background tasks and close after Fiber shutdown.
- Redis uses Fiber Storage and Uptime's native backend, never a custom Store.
  Close the borrowed Fiber handle before the owned go-redis client. No backend
  migration, fallback, reset, dual-write, or distributed scheduling.
- Target-service failure is monitoring data; Uptime runtime failure is operational health.
- No hot reload, management UI, user account system, alerting, APM, or unrelated
  monitoring expansion without an explicit architecture decision.
- Preserve MIT `LICENSE`; use [LibreCosta organization community files](https://github.com/librecosta/.github) rather than
  adding local Code of Conduct, Contributing, or Security copies.
- Do not create empty future packages or advertise unimplemented CLI commands.

## Verification

Run `make check` before completing a change. When GNU Make is unavailable, use the
equivalent Go commands in [.agents/playbook.md](.agents/playbook.md) and verify
formatting leaves a clean diff. Report what actually ran and any gaps.
For storage or runtime changes also run `make race`; race is deliberately separate from
the fast canonical check and runs in a dedicated Go 1.27.x CI job.
