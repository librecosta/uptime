# Alert Contract (v0.2.0-B1/B2/C1)

`internal/alerts` is an independent, observation-driven core. `internal/app`
optionally feeds it upstream endpoint probe results. Alerts are disabled by
default: ordinary startup creates neither an alert database nor an alert worker.
No notification delivery, channel configuration, or management API exists.

## State and observations

- HEALTHY -> PENDING on an actual failed observation; the third consecutive
  failure confirms FIRING by default. An explicit threshold must be 1 through 10.
  Threshold one confirms immediately. A successful observation resets PENDING.
- Only real success closes FIRING and produces RECOVERED. Repeated failures in
  FIRING and repeated success after recovery create no additional transitions.
- A Gap is a standalone signal, not a probe. Its success/status fields are
  ignored. It resets unconfirmed failure counts even if its timestamp is old,
  but never automatically closes an incident. Shutdown, storage errors, and
  removal from configuration are not observations and generate no transitions.
- Inputs contain endpoint ID, completion time, success, HTTP status, and Gap.
  HTTP status is informational: the caller supplies the probe's success decision.
  A zero timestamp uses the injected clock (default `time.Now`); times from
  probes are otherwise preserved and normalized to UTC. No real-time wait is
  required by the state machine.
- The caller supplies ordered observations. Within one engine, older completion
  timestamps are ignored, except Gap. Equal timestamps can represent distinct
  real probes and are consumed; timestamps do not identify or deduplicate input
  observations. This is not a durable event log or a multi-writer event stream.
- PENDING exists only in memory and resets on restart. Before consuming an
  observation the engine reads the endpoint's active incident to restore FIRING.
  This is lazy recovery, not an automatic startup scan or a synthetic probe.
  An unreadable store rejects the observation and breaks an unconfirmed sequence;
  it does not invent a failed probe or close a persistent incident.

## Incident and simulated outbox

- A confirmed incident has a cryptographically random stable ID, endpoint ID,
  first failure and confirmation timestamps, optional recovery timestamp, and
  immutable channel ID snapshot. The channel snapshot is taken at confirmation,
  normalized into sorted unique IDs, and reused for recovery regardless of
  subsequent policy changes. IDs use 1–128 ASCII letters/digits/`_`/`.`/`-`,
  starting with a letter/digit; channel credentials have no representation here.
  An empty channel snapshot is valid and produces no tasks.
- One endpoint has at most one active incident. A transaction atomically creates
  that incident, its active index, and one DOWN task per channel. Recovery
  atomically closes the same incident, clears its active index, and creates its
  RECOVERED tasks. Invalid transitions fail without partial application.
- Task EventID is SHA-256 of incident ID, transition type, and channel ID separated
  by NUL bytes. Task identity is independent of attempt time. Every task contains
  incident/channel IDs, type, occurrence time, and delivery state. New tasks start
  PENDING unless their DOWN dependency has already failed or been skipped.
  No claim, network destination, response body, token, or secret is persisted.
- Reapplying a committed transition returns its durable incident and creates no
  tasks. Retrying DOWN for an already closed incident never reopens it. Mutating
  an existing incident's immutable fields is a conflict. Outbox queries return
  all tasks sorted by EventID; they do not form a transaction with separate reads.
- A failed write may have committed. The engine reads the same incident ID and
  verifies the immutable facts and, for recovery, the exact recovery timestamp.
  If the outcome remains unknown, it returns the error and retains the original
  transition/ID in memory for reconciliation on the next explicit Observe call.
  A retried transition represents the original real observation, not that next
  call's gap or probe. There are no background retries. On restart, only durable
  facts survive; an uncommitted in-memory transition cannot be reconstructed.

## Persistence and ownership

- The bbolt database is independent of `uptime.db`: explicit path (recommended
  `./data/alerts.db`), format `librecosta-uptime-alerts`, Schema v1. `meta/format`
  is UTF-8, `meta/schema_version` is an eight-byte big-endian uint64 equal to 1.
  Required buckets are `meta`, `incidents`, `active`, and `outbox`. Records use
  JSON; the active bucket maps endpoint IDs to incident IDs.
- Only a definitively newly created file is initialized. Existing empty, corrupt,
  foreign, or unsupported-schema files are refused, never adopted, migrated,
  repaired, or removed. File mode is 0600 and new parent mode is 0750. Operations
  validate metadata and accessed records. bbolt transactions provide locking and
  rollback across incident/index/tasks. Close is idempotent; callers stop use
  before closing. Contexts do not promise to interrupt file locking or disk I/O.
- Redis borrows a caller-owned standalone `*redis.Client` in the selected database.
  The caller closes it after use. Construction performs no I/O. The caller supplies
  a deployment prefix; only `<prefix>:alerts:v1:{incidents,active,outbox}` hashes
  are accessed. These names are independent of Uptime's native key schema; no
  monitoring key, other namespace, or global reset operation is used.
- Redis WATCH checks the incident, active index, key types, and task identities;
  MULTI/EXEC commits all changes. Type checks happen under WATCH because Redis
  does not roll back runtime command errors. Conditional conflicts retry at most
  32 times; network errors are returned for reconciliation, not blindly retried
  by this core. Atomic active reads also verify the watched index/record snapshot.
  Server durability still depends on the operator's Redis persistence settings.
- No alert key has an application TTL. Active incidents and PENDING tasks remain
  until a future explicit policy exists. B1 performs no cleanup, retention sweep,
  implicit endpoint deletion, or archival. Future retention must preserve active
  incidents and undelivered tasks, with versioned migrations for schema changes.

## Runtime ingress and lifecycle (B2)

- Optional YAML `alerts.enabled` defaults to false; `consecutive_failures`
  defaults to three and must be an integer 1 through 10, even when disabled.
  Config check remains offline and never opens alert persistence. Enabled alerts
  apply to every configured endpoint with an empty channel snapshot, so incidents
  are durable but no outbox tasks are created by the runtime.
- The pinned upstream `OnProbeResult` is the only event source. Its callbacks
  are ordered per endpoint and concurrent across endpoints. The adapter assigns
  an endpoint-local sequence before a nonblocking enqueue to a 256-entry channel.
  It does no storage, network, or logging I/O and starts no probes. Completed
  results are accepted even if the callback's parent context is already canceled.
- A full buffer drops the incoming event and counts the loss. One worker emits
  aggregated, credential-free warnings at most once per five seconds, plus a final
  pending-loss report on exit. The ingress is not a durable event stream.
- The worker serializes calls to the existing Engine. Missing sequence numbers
  cause a Gap before the next real observation, using that observation's exact
  completion time. Failed store operations do not acknowledge the event; the next
  observation must pass a Gap again. No failures are accumulated across missing
  observations, and no store error is converted into DOWN or RECOVERED. The
  engine retains and reconciles uncertain transitions from earlier real probes.
  Each observation uses `server.shutdown_timeout` as its storage-operation budget;
  this supplies a deadline even to client I/O already in flight at shutdown.
- Enabled bbolt alerts use `alerts.db` beside the selected monitoring file.
  Identical paths and existing file aliases are rejected, never shared or migrated.
  Redis alerts borrow the already-owned standalone client, database, and deployment
  prefix. A read-only startup check rejects incompatible alert key types; transition
  validation remains transactional. No second client/pool is created for alerts.
- Alert persistence and the worker are initialized before `uptime.New`, which may
  begin probing during construction. Alert initialization failures abort startup
  and release acquired resources. TLS, auth, dashboard, monitoring persistence and
  readiness retain their existing contracts; alert availability does not redefine
  `/readyz` or turn target failures into process failures.
- Shutdown first cancels and joins upstream probes through Fiber's existing hooks,
  then stops ingress, drains accepted events, joins the worker, and closes alert
  persistence before monitoring resources. Drain cancellation is independent of
  probe cancellation and uses `server.shutdown_timeout`; expiry stops further
  processing and returns a safe timeout error. Uninterruptible storage I/O may
  exceed that deadline: resources are never closed underneath a running operation.
  Accepted in-memory events are not guaranteed to survive process termination or
  an expired drain deadline. Shutdown itself never generates a recovery.

## Delivery persistence (C1)

- `DeliveryStore` is separate from the incident `Store`: `GetTask(EventID)`,
  `QueryDue(DueQuery)`, and `RecordOutcome(Outcome)`. Both existing backends
  implement it. Nothing in C1 sends notifications, polls the outbox in a worker,
  claims tasks, or changes B2's empty runtime channel snapshot.
- Tasks have PENDING, SENT, FAILED, or SKIPPED status. Only PENDING can accept a
  new outcome; terminal states never reopen. Added JSON fields are `attempts`,
  `next_attempt_at`, `last_attempt_at`, `finished_at`, `last_error_code`,
  `last_attempt_id`, and `attempt_receipts`. Missing fields in B1/B2 PENDING
  records mean zero attempts, no receipts, and initially due at `occurred_at`.
  This is additive JSON within the same alerts Schema v1, buckets and Redis
  hashes, without rewriting old records on open. EventID and channel snapshots
  remain unchanged. Legacy `Outbox` still returns all tasks, now in all states.
  Compatibility is C1 reading old records; older binaries do not understand the
  new terminal delivery states.
- An Outcome supplies EventID, a task-local unique AttemptID, ExpectedAttempts
  (the count before this attempt), explicit completion time AttemptedAt, status,
  classified ErrorCode, and optional RetryAt. The completion must not precede
  the task's due time or last completion. SENT has no error code. Other outcomes
  require one of the fixed codes declared in `delivery.go`; raw errors, URLs,
  credentials and response bodies are rejected rather than sanitized into storage.
  Attempt IDs must be opaque generated IDs, never request or credential data.
- Accepted outcomes increment Attempts once and set LastAttemptAt/LastAttemptID.
  SENT, FAILED and explicit SKIPPED set FinishedAt and clear NextAttemptAt.
  PENDING schedules the next attempt; the tenth PENDING outcome instead becomes
  FAILED. Automatic dependency skips make no attempt and preserve a zero count.
- `RetryTime` is pure: after completed attempts 1..9, delay is
  `min(10s * 2^(attempts-1), 30m)`. A valid later RetryAt can extend beyond 30m;
  an earlier future hint cannot shorten the ordinary backoff. Past/invalid hints
  are rejected. Attempt ten has no next time regardless of a later hint. No
  sleeping, jitter, actual retries or retention scheduling is implemented.
- Conditional updates check both the expected count and AttemptID. Up to ten
  bounded receipts store each AttemptID and SHA-256 digest of its normalized
  Outcome (UTC timestamps). Exact duplicates, including older attempts after a
  newer result or terminal transition, return current task state without mutation.
  Changed results, reused IDs or stale counts are conflicts. Receipts survive
  restart; they do not record external response data or provide external exactly-once
  delivery. After an uncertain commit, read GetTask then use Task.HasOutcome,
  or replay the identical Outcome. Failed/unavailable reads prove neither commit
  nor rollback. Storage errors are not delivery outcomes and do not advance counts.
- A RECOVERED task is eligible only after its same-incident, same-channel DOWN
  is SENT. RecordOutcome enforces this as well as QueryDue. A final FAILED DOWN
  atomically turns an existing PENDING recovery into SKIPPED with code
  `dependency_failed`. If recovery is created later, the incident transaction
  creates it already SKIPPED. An explicitly SKIPPED DOWN similarly uses
  `dependency_skipped`; no permanently blocked recovery can be sent. Automatic
  skip FinishedAt is the later of recovery OccurredAt and DOWN FinishedAt.
  Other channels and incidents are unaffected. Missing/corrupt dependencies
  reject the whole write without partial incident or task updates.
- bbolt uses one Update transaction. Redis reads under existing WATCH and commits
  an outcome plus dependent skip in one HSET inside MULTI/EXEC; recovery creation
  uses the existing incident transaction and watches the same hashes. Both event
  creation orders and racing writes therefore follow identical conditional rules.
  No Redis keys, application TTLs, schemas, locks, or leases are added.
- QueryDue requires explicit Now, a backend-specific opaque cursor, and Limit
  1..100. Each call evaluates at most Limit scanned records and returns at most
  Limit eligible tasks. Pages can be empty with a nonempty NextCursor; callers
  must finish the pass and use the same Now. bbolt traverses EventID key order;
  Redis uses HSCAN with COUNT and a batch offset, never new HGetAll queries.
  Redis COUNT is a server hint, so a larger raw response is resumed across pages;
  it is not a hard bound on Redis response bytes. Pages have no cross-call snapshot
  or ordering guarantee across backends. Concurrent writes/rehashing can repeat or
  displace records; later passes start at an empty cursor. A page is advisory,
  never a reservation, and RecordOutcome must recheck conditions.

## Boundary and verification

One alert executor is supported. The engine serializes its calls; both stores
protect concurrent conditional operations, but this is not leader election,
leases, voting, a probe scheduler, or a delivery guarantee. Shared Redis writers
must be coordinated externally. Channel configuration, real delivery, retry
scheduling, routing, and retention scheduling remain outside C1.

`make check` covers deterministic state and real temporary bbolt tests. `make race`
includes the alert package and runtime adapter. Runtime tests include real local
HTTP probes, bounded ingress/gaps, store faults/reconciliation, restart and shutdown
draining. C1 exercises shared backend delivery rules, bounded pagination,
conditional races, retry exhaustion, legacy records, reopen and uncertain-result
reconciliation; bbolt rollback tests use real temporary databases. Real Redis tests
use `UPTIME_TEST_REDIS_URL` and unique
owned prefixes, skip explicitly without that variable, and delete only their
known keys. The existing Redis integration CI job runs them alongside app tests.
Report skipped Redis checks separately from actual integration passes.
