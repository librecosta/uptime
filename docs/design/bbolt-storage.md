# bbolt Storage Design

Status: implemented in P1 and integrated into the standalone runtime in P3.

This document develops the storage rationale in the
[initial product/technical baseline](v0.1.0-product-technical-design.md), sections
15–20 and 26–27. Stable invariants live in the
[persistence contract](../../contracts/persistence.md). The schema below describes
the implementation in [storage/bbolt](../../storage/bbolt/). It is versioned
internal persistence, not a third-party bucket API or an announced product release.

## Default Backend and Public Package

bbolt provides embedded persistence in one local file, matching the product's
one-binary, zero-external-infrastructure default. Explicit transactions allow
slot deduplication, counters, and related metadata to change atomically. This
choice accepts single-process file ownership and offline maintenance rather
than introducing a database service for the default deployment.

The `github.com/librecosta/uptime/storage/bbolt` package is public so a
third-party Fiber application can reuse it without running the standalone
product. It implements Fiber Contrib Uptime's exported `uptime/storage.Store`
contract and never imports this repository's `internal/*` packages. A compile-time
assertion checks Fiber Contrib Uptime v0.2.2's public interface. The only other
direct dependency is bbolt v1.5.0.

The old Redis-emulation approach mentioned by the product baseline is rejected:
a direct Store implementation expresses service, heartbeat, and daily semantics
without translating embedded data through a Redis-shaped compatibility layer.
This decision follows the in-repository baseline and requires no prototype code.
P4 optional Redis persistence uses Fiber Storage Redis and Uptime's native Redis
backend directly. It does not alter this schema or the public bbolt API. Selecting
a backend does not migrate, merge, delete, or dual-write history, and failures
never fall back to another backend.

## Schema v1

```text
uptime.db
├── meta
│   ├── format
│   └── schema_version
├── services
│   └── <service-id>
│       ├── name
│       ├── description
│       ├── created_at
│       ├── last_seen_at
│       └── sample_interval
├── instances
│   └── <instance-id>
│       ├── service_id
│       ├── hostname
│       ├── pid
│       ├── started_at
│       └── last_seen_at
├── samples
│   └── <service-id>
│       └── <YYYY-MM-DD>
│           ├── up_slots
│           └── slots
│               └── <slot-key>
└── daily
    └── <service-id>
        └── <YYYY-MM-DD>
            ├── up_slots
            ├── expected_slots
            └── finalized
```

Service IDs are opaque, non-empty persistent identities; no standalone YAML ID
regex is applied and IDs are not escaped or hashed. Names and descriptions can
change; meaningful `CreatedAt` cannot. Instance `StartedAt` is stable, while its
service association, hostname, and PID may refresh. Both entities' `LastSeenAt` take the maximum of
existing and incoming timestamps. Active/detached status is derived from current
configuration and stored service IDs; it is not a persisted boolean.

Creation/start timestamps fall back to incoming `LastSeenAt`. If both are zero,
the creation/start field remains absent until a meaningful upsert. A zero
incoming last-seen value never moves an existing value backwards. Missing display
name falls back to service ID; missing optional metadata is decoded as zero.
`sample_interval`, daily fields, and both sample-day members are required.

Use raw UTF-8 strings, eight-byte big-endian int64/uint64 values, UnixNano int64
timestamps, nanosecond int64 durations, and single-byte booleans. Instance and
slot keys use big-endian integers; days use ASCII `YYYY-MM-DD`, interpreted in
the configured timezone. Avoid JSON, Gob, Protobuf, or MsgPack inside the DB.
Signed numbers use their int64 bit representation. Go `int` fields are checked
against the host architecture's range before narrowing. Zero time is encoded as
int64 zero, which decodes as `time.Time{}`; Unix epoch nanosecond zero is therefore
the same sentinel, matching upstream. Out-of-range UnixNano timestamps are rejected.
All accepted non-empty days must be calendar-valid canonical `YYYY-MM-DD`.
Slot keys must be non-negative; slot values are a one-byte `0x01` marker.

Decoders reject wrong widths, invalid booleans, wrong bucket/value types, missing
required fields, and malformed slot/day keys with contextual errors when those
fields are accessed or validated during maintenance. Hot paths do not scan
unrelated historical slots. No operation automatically repairs damaged records.
Ordering is an internal testing convenience, not a public compatibility guarantee.

## Transactions and Heartbeat Deduplication

Use short explicit read/write transactions. Do not retain transaction-owned
values after the transaction ends; copy data needed by callers. Failed writes
must roll back related changes together.

Heartbeat identity is `(ServiceID, Day, Slot)`. In one `db.Update`:

1. Advance existing service and instance `LastSeenAt` monotonically.
2. Insert the slot key only if absent; otherwise require its marker to be `0x01`.
3. Increment the day's `up_slots` only for a newly inserted slot.

This makes retries idempotent and keeps the redundant count consistent with
stored slots. One slot uses one key; bitmap optimization is deferred. Upstream
day/slot semantics remain authoritative. Heartbeats can create samples without
registration but cannot invent service or instance metadata. Registration belongs
to `UpsertService` and `UpsertInstance`.

`readSampleCount` is the lightweight read used by `WriteHeartbeat` and
`QueryTodaySamples`: require `up_slots`, strictly decode a non-negative count
within the platform's `int` range, and require a `slots` bucket. These hot paths
trust the persisted count without walking historical slots. Heartbeats inspect
only their current slot and reject an invalid existing marker or nested bucket;
a new slot and the counter increment commit atomically.

`validateSampleDay` additionally scans every slot key and marker and compares
the actual count with `up_slots`. `RollupDaily` uses it when collecting sample
days and again before finalizing a candidate; `Cleanup` uses it for raw sample
days before the retention boundary. Malformed keys, negative slots, invalid
markers, and count mismatches return contextual errors without automatic repair.
Unrelated historical corruption can remain undetected by hot paths and is
detected by this full validation during maintenance. Behavioral fixtures test
this distinction without timing benchmarks, alongside duplicate, out-of-order,
and concurrent writes and counter consistency.

## Rollup, Finalization, and Cleanup

Rollup processes days strictly before `BeforeDay`. Once a daily row is finalized,
it cannot be overwritten, even when a late heartbeat adds raw slots. A read
transaction collects eligible registered service/day candidates, then closes.
`ExpectedSlots` runs synchronously outside all transactions and is not retained;
nil means zero expected slots. A negative result is rejected.

A short write transaction per candidate rechecks service/sample existence and
finalized state, then reads the latest count before finalizing. Reentrant Store
reads/writes in callbacks are supported. Concurrent rollups cannot overwrite the
first finalized result, and removal cannot be undone by a stale candidate.
Rollup is atomic per day, not across the whole call; retrying after an error is
safe. Rollup does not immediately remove raw samples.

Cleanup removes raw samples only when all conditions hold:

- The day is strictly before `SamplesBeforeDay`.
- Its corresponding daily row exists.
- That row is finalized.

Apply this guard before pruning daily history using `DailyBeforeDay`; an
unfinalized day's necessary raw samples must survive. All three cleanup dimensions
run in one write transaction: raw samples, old daily rows, then instance metadata
whose activity is strictly before the current time minus 24 hours. Activity is
`LastSeenAt`, falling back to `StartedAt` only when last-seen is zero. Instances
with both timestamps zero and activity exactly at the cutoff are retained. This
preserves Fiber Uptime v0.2.2 endpoint instances registered with `StartedAt = now`
and zero `LastSeenAt` before startup maintenance and the first probe. An
unexported clock seam makes tests deterministic; no worker runs.
Empty history boundaries disable that dimension. No service registration is deleted.

## Queries

Nil ServiceIDs select registered services; a non-nil empty slice selects none.
Explicit selections access history by ID and omit absent IDs/days. Daily bounds
are inclusive, with empty ends unbounded. An empty sample query day returns no
rows, and zero-up raw rows are omitted. Malformed fields accessed by the query
return errors without partial result slices; current-day queries validate the
counter and required bucket, not every slot. Result ordering remains unspecified,
as upstream requires.

## Explicit Service Removal

`RemoveService` deletes the service, its raw samples, its daily rows, and all
instances associated with its ID in one write transaction. Failure cannot leave
a partially removed history.

The application enforces the planned prohibition on removing services still
active in YAML; the storage package does not import application configuration.
Deleting an endpoint from YAML only stops future observation. It never invokes
history deletion implicitly. Maintenance commands use public storage operations,
not raw bucket access.

## Context and File Ownership

bbolt transactions do not accept a `context.Context`. Cancellation is best
effort: check `ctx.Err()` before entering a transaction and periodically during
long loops. Do not promise that context cancellation can precisely interrupt
writer-lock waiting or an in-flight transaction. The open timeout bounds
file-lock acquisition; it is not a general context-cancellation guarantee.

The product uses a single process to own a bbolt file. A second process must not
open that same file for concurrent product operations. Consequently bbolt
service list/export/remove commands are planned as offline maintenance: stop
the application, perform the command, then restart. No IPC or admin HTTP control
plane is introduced to bypass this boundary.

## Schema Evolution and Readiness

New databases use raw `meta/format = deepfurry-uptime-bbolt` and eight-byte
big-endian uint64 `meta/schema_version = 1`. Open a supported version;
refuse a newer unsupported schema without rewriting it. Older released schemas
need explicit supported forward migration before use. Missing or malformed
metadata in an existing database is an error, not permission to reinitialize or
discard it. Both older and newer unsupported versions fail in P1. A future v2
can add a concrete v1-to-v2 migration; no generic migration framework exists.

`Open` uses exclusive OS creation to establish ownership of a genuinely new file.
Existing files are opened without creation and must be non-empty regular files.
Schema initialization is a single transaction on an otherwise empty new DB.
bbolt's automatic opening-time freelist write is suppressed until schema validation
finishes; normal freelist syncing is then restored before publishing the Store.
Thus invalid existing databases, including unrelated bbolt files, are not rewritten.
On failed new-file initialization the handle is closed and only that same created
file is eligible for best-effort removal. Existing permissions are preserved.

`Ping` uses a lightweight read transaction to verify an open handle, the format
marker, supported schema version, and all five top-level buckets. It is not an
fsck and does not scan record history. It creates no temporary write data and
does not promise the next write will succeed. Backend failures never switch
silently to in-memory persistence.

## Lifecycle and Backup

The caller opens the Store before giving it to Fiber Uptime and owns its close.
The Store contract does not transfer resource ownership to the middleware.
The P3 app opens bbolt and binds a listener before constructing Fiber Uptime,
passing the public Store through `uptime.Config.Storage`. The once-only shutdown
path clears readiness and calls Fiber ShutdownWithTimeout; Uptime's pre-shutdown
hook cancels/waits workers. HTTP must finish before Run's deferred cleanup closes
storage. On timeout, remaining connection I/O is forced closed and handlers drain;
no OnPostShutdown callback closes the DB prematurely. Construction/bind failures
also release resources, and cleanup errors are joined with the original failure.
`Close` is idempotent and concurrent calls share its first result; operations
after close return errors. No storage schema or public API change is needed.

The public package requires an explicit non-empty `Config.Path`; it has no
product default. A zero `Timeout` defaults to five seconds; negative values
fail. Missing parent directories use `0750` and new files use `0600` where Unix
permissions apply. Existing modes are preserved. The standalone default path is
`./data/uptime.db`, and failure to open its backend is fatal at startup.

Use a cold backup for v0.1.0: gracefully stop the application, copy the complete
database, then restart. An ordinary copy during writes is not a promised
consistent backup. Online transactional backup can be designed later if needed.

A service archive is a versioned, human-readable history export assembled through
Store queries, independent of raw bbolt layout. It excludes secrets and process
instance metadata. It is not a database backup and v0.1.0 provides no archive
import/restore. Third-party consumers use public Go APIs or documented archives,
not direct bucket edits.
