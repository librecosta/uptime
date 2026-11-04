# Alert Contract (v0.2.0 and v0.2.1-B1)

`internal/alerts` is an independent, observation-driven core. `internal/app`
optionally feeds it upstream endpoint probe results. Alerts are disabled by
default: ordinary startup creates neither an alert database nor an alert worker.
`internal/notify` provides HTTP adapters; `internal/app` composes optional channel
configuration and a single delivery worker. There is no management API.

## State and observations

- HEALTHY -> PENDING on an actual failed observation; the third consecutive
  failure confirms FIRING by default. An explicit threshold must be 1 through 10.
  Threshold one confirms immediately. A successful observation resets PENDING.
- FIRING closes only after `Policy.RecoverySuccesses` consecutive real successes
  (zero selects one; explicit 1..10). YAML `alerts.recovery_successes` defaults
  to one and accepts 1..10. Failures, any Gap and restart reset the in-memory
  recovery count. Default behavior matches v0.2.0. Repeated failures in
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

## Incidents and outbox

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
  WATCH does not isolate intermediate reads: logical validation errors are checked
  with a read-only EXEC before returning, so concurrently invalidated conditions
  retry rather than falsely reporting a stable conflict or corrupt record.
  Server durability still depends on the operator's Redis persistence settings.
- No alert key has an application TTL. Active incidents and PENDING tasks remain
  until a future explicit policy exists. B1 performs no cleanup, retention sweep,
  implicit endpoint deletion, or archival. Future retention must preserve active
  incidents and undelivered tasks, with versioned migrations for schema changes.

## Runtime ingress and lifecycle (B2)

- Optional YAML `alerts.enabled` defaults to false; `consecutive_failures`
  defaults to three and must be an integer 1 through 10, even when disabled.
  Config check remains offline and never opens alert persistence. Enabled alerts
  apply to every configured endpoint, with independently resolved channel policies
  (an empty snapshot still tracks incidents without creating tasks).
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
  claims tasks, or determines the runtime channel policy.
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

## Notification adapters (C2)

- `internal/notify.Sender.Send(ctx, Message)` performs at most one HTTP attempt.
  Message contains a persisted Task, its Incident, and an optional display name
  (falling back to Endpoint ID). Both adapters check PENDING eligibility, frozen
  EventID, incident/channel association, channel snapshot membership, and event
  timestamps before I/O. DOWN occurrence equals confirmation; RECOVERED requires
  the matching recovery time. Terminal tasks and inconsistent inputs are refused.
  The caller still owns due-time/dependency checks and immutable input snapshots.
- A Result contains TaskStatus, classified ErrorCode, UTC completion AttemptedAt,
  and optional RetryAt. PENDING means retryable, SENT means an acknowledged
  delivery, and FAILED means permanent rejection. There is no RecordOutcome call,
  attempt ID generation, scheduling, task scan, background worker or implicit
  adapter retry. C1 remains responsible for backoff, exhaustion and persistence.
- Invalid configuration/message construction returns a fixed sentinel and no
  result. A pre-expired context returns its context error without sending. An
  in-flight parent cancellation returns `context.Canceled` and no result; timeout
  during an attempt is PENDING/`timeout`. A fully read response remains a fact
  even if shutdown follows. Cancellation/network failure can occur after remote
  acceptance: stable EventID supports receiver deduplication, not exactly-once
  delivery. Callers must not turn a zero result into success or a stored outcome.
- Requests use standard-library HTTP, verified TLS and no redirects/cookies.
  Default total timeout is 10s, configurable internally up to 30s; a shorter
  injected client timeout is retained. Response bodies are read through a 64 KiB
  limit and always closed; oversized responses fail with `rejected`. Known
  transports with TLS verification disabled are refused. Injected custom
  transports/clocks must be concurrency-safe; transports must honor contexts and
  must not add redirects, retries or sensitive logging. Configuration, raw HTTP
  errors, request/response bodies and credentials are never logged or returned.
- Generic Webhook POSTs JSON with `schema_version: 1`, `event_id`, `incident_id`,
  `event_type`, `endpoint_id`, `endpoint_name`, `occurred_at`, `first_failure_at`,
  and `confirmed_at`; RECOVERED adds `recovered_at`. All times are UTC. A delayed
  DOWN remains unchanged when its incident later recovers. Payloads are limited
  to 64 KiB. Headers are `Content-Type: application/json`,
  `X-LibreCosta-Event-ID`, and optional Bearer Authorization; arbitrary headers
  are unsupported. Non-loopback destinations with a token require HTTPS;
  localhost and literal loopback IPs allow HTTP. Userinfo/fragments are rejected.
- Generic 2xx succeeds. HTTP 408/429/5xx yields PENDING with `timeout`,
  `rate_limited`, or `unavailable`; network failures use `network` (timeouts use
  `timeout`). Other HTTP statuses, TLS certificate failures and oversized
  responses are FAILED/`rejected`. One valid Retry-After delta or HTTP date may
  provide a future lower bound, at most 24h from completion. Invalid, duplicate,
  past and excessively distant hints are ignored; hints never shorten C1 backoff.
- Feishu sends only custom-bot text, including DOWN/RECOVERED, Endpoint name/ID,
  UTC event time and Incident ID. An optional keyword is prepended. Only exact
  `open.feishu.cn` and `open.larksuite.com` HTTPS hosts (default/443 port), with
  `/open-apis/bot/v2/hook/<token>` and no query/fragment, are accepted. Serialized
  JSON is limited to 20 KiB. With a Secret, each Send calculates fresh Unix-second
  timestamp and Base64 HMAC-SHA256 of the empty message using
  `timestamp + "\n" + secret` as the key, per the
  [official Feishu protocol](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot).
- Feishu requires an explicit integer `code: 0` or legacy `StatusCode: 0` in a
  2xx response. Missing, malformed, duplicate or conflicting codes cannot succeed.
  Code 11232 is PENDING/`rate_limited`; 19001/19021/19022/19024 are permanent
  `rejected`. Other business errors or malformed JSON on 2xx are rejected;
  other HTTP failures retain HTTP classification unless a known business code
  applies. Redirects are always rejected, and HTTP failures cannot be rescued by
  a success body. No SDK, OAuth or application-bot behavior is implemented.

## Configuration and delivery runtime (C3)

- Optional `alerts.channels` is a sequence of unique literal IDs matching the
  existing channel identity grammar. `type` selects `webhook` with nested `url`
  and optional `bearer_token`, or `feishu` with nested `url`, optional `secret`
  and `keyword`. Enabled alerts resolve environment variables only in the active
  adapter branch. Disabled alerts leave channels/routes unresolved and allocate
  no sender or worker; strict YAML shape/unknown-key checking still applies.
  Adapter validation is pure and shared with offline configuration checks.
- Endpoint `alert_channels` omission selects all configured channels, explicit
  `[]` selects none, and a list must contain unique references to defined IDs.
  IDs/types/references are literal, not environment templates. Each endpoint has
  its own Policy; confirmation freezes its channel snapshot. Recovery always
  reuses that snapshot, even after configuration changes. Never repurpose an ID
  for an unrelated receiver; old tasks resolve that ID through current config.
- One worker scans immediately at startup and then once per second. QueryDue
  uses pages of 50 scanned records and at most eight pages per tick; a pass keeps
  the same Now/cursor across ticks, including empty pages with a NextCursor.
  Each candidate is re-read before sending to avoid repeats from stale pages.
  C1 remains responsible for eligibility, DOWN-before-RECOVERED, conditional
  outcomes, backoff, ten-attempt exhaustion and dependent skips.
- Read the associated Incident and use the current endpoint display name, or
  Endpoint ID if it is no longer configured. A missing sender retains its task
  unchanged and emits a warning at most once per five seconds. It is never
  rerouted, marked SENT, skipped or discarded because configuration is missing.
  Storage/incident-read errors also defer; they are not network outcomes.
- Each completed Send gets one cryptographically random task-local AttemptID
  and one immutable Outcome. On a failed RecordOutcome, GetTask/HasOutcome checks
  its receipt. If still uncertain, retain the identical Outcome in memory and
  replay only its conditional record on later ticks; no more sends occur until
  reconciliation succeeds. A storage fault never causes an immediate resend or
  increments attempts by itself. Cancellation/pre-send validation errors from
  the adapter return no result and do not create a synthetic attempt.
- Feishu channels for the same official host/hook path share one rate gate:
  at most one send per second, without bursts. A later provider RetryAt extends
  the gate for every channel referencing that bot. Deferral consumes no attempt
  and does not block other channels. No limiter dependency or sleeping retry
  loop is introduced. Failure logs contain only quoted task/channel IDs and
  fixed error classifications; no raw HTTP/storage errors or credentials.
- Startup opens monitoring/alert persistence, validates and creates senders,
  prepares ingress and starts the delivery worker before constructing Uptime.
  Its initial pass resumes persisted PENDING tasks, including detached endpoints.
  Notification availability never changes readiness, UI or probe behavior.
- Shutdown first joins upstream probes and drains ingress, then cancels/joins
  delivery, releases its idle HTTP connections and closes alert storage, finally
  monitoring storage. Completed sends use an independent, shutdown-timeout-bounded
  recording context so a racing HTTP cancellation does not erase a result.
  Workers always exit before database closure; uninterruptible I/O can exceed
  the timeout. Unfinished delivery tasks remain persisted for the next startup.
- Delivery has at-least-once semantics with bounded attempts, not exactly-once or
  guaranteed receipt. A receiver can accept a message before a response is lost,
  and a crash can lose an unrecorded in-memory Outcome. A later attempt may repeat
  that message; Generic Webhook receivers should deduplicate stable EventID.
  SENT means adapter acknowledgement, not a human seeing it. One alert executor
  must own each bbolt file/Redis namespace; no claims, leases or distributed
  arbitration are implemented. Back up alerts.db alongside monitoring history
  while stopped. No schema, TTL or cleanup policy changes accompany C3.

## Deployment and backup acceptance (D1)

Stop the process before copying bbolt files. Preserve `uptime.db` and its sibling
`alerts.db` as one backup set whenever the latter exists, even with alerts now
disabled. Restore both with service ownership and mode 0600; do not mix snapshots
or leave a newer alert file beside restored monitoring history. Existing schemas
are unchanged, and no migration, automatic backup or retention task is added.
Redis uses `<monitoring prefix>:alerts:v1:` in the configured database; Redis's
own coordinated RDB/AOF backup must include monitoring and alert state together.
External stop budgets must allow separate shutdown, drain and outcome-recording
timeouts before storage closure. The deployment examples allow 45s for the default
10s application timeout; uninterruptible I/O still cannot guarantee a hard bound.
These in-process alerts cannot detect the monitor's complete process failure.

## Recovery stability and reminder core (v0.2.1-B1)

- Recovery counts are volatile, including while a persistent incident is FIRING.
  Only reaching the success threshold requests the existing atomic recovery
  transition; pending/uncertain transition reconciliation remains unchanged.
- `Incident.ReminderPolicy` freezes a positive interval and maximum count.
  Nil means disabled; legacy incidents have no schedule and never acquire one
  implicitly. `ReminderCount` counts issued reminders, not sends. An active,
  non-exhausted snapshot has `NextReminderAt`; recovery/exhaustion clears it.
- Pure `PrepareReminder` accepts only a matching real failed observation at or
  after the saved deadline. Success, Gap, a closed incident and exhausted/disabled
  policies produce nothing. A long outage produces at most one reminder; the next
  interval starts at the actual observation completion time, with no catch-up.
  Each snapshotted channel gets a PENDING task with positive `ReminderSequence`.
- Reminder EventID is SHA-256 of `IncidentID + NUL + "REMINDER" + NUL + decimal
  sequence + NUL + ChannelID`, without padding. DOWN/RECOVERED IDs are unchanged;
  their sequence remains zero. Generic JSON adds `reminder_sequence` only for a
  reminder, using `occurred_at` as its event time. Feishu adds a sequence line;
  the original DOWN/RECOVERED payloads are unchanged.
- Pure `DeliveryReady`/`PrepareDeliveryOutcome` require a consistent incident and
  same-channel dependency snapshot: DOWN must be SENT, reminder N waits for N-1
  to become terminal (SENT/FAILED/SKIPPED), and recovery waits for the last issued
  reminder. Missing/corrupt dependencies reject; failed DOWN cannot authorize a
  reminder. Existing DOWN-failure recovery skips retain their v0.2.0 rules.
- `CancelReminder` prepares recovery's cancellation of PENDING reminders,
  including those with previous attempts. It uses SKIPPED/`incident_recovered`,
  clears the retry time and preserves Attempts, LastAttemptAt/ID and every receipt.
  FinishedAt is the later of recovery and the last attempt; no attempt is added.
  Already terminal tasks are unchanged. This reason cannot be supplied as a send
  Outcome. B2 must commit cancellation with recovery in the same transaction.
- A completed send racing cancellation still attempts conditional recording.
  On conflict, Worker reads the task and closed incident. A valid cancellation
  with unchanged pre-send count releases its fence without claiming an outcome
  receipt or overwriting SKIPPED. Unavailable, mismatched or invalid reads retain
  the same Outcome/AttemptID for reconciliation and never authorize a resend.
- **B1 does not enable reminder persistence or runtime scheduling.** Schema v1
  rejects reminder snapshots/transitions/tasks through its shared validation.
  There is no Reminder YAML, queue, timer, new Redis key/index or schema upgrade.
  New fields omit zero values, preserving legacy JSON. Engine creates only
  DOWN/RECOVERED. Adapter reminder sends are exercised only with local/injected
  transports; B2 must add Schema v2, migration and atomic reminder/cancel writes
  before wiring the pure core into the live runtime.

## Boundary and verification

One alert executor is supported. The engine serializes its calls; both stores
protect concurrent conditional operations, but this is not leader election,
leases, voting, a probe scheduler, or a delivery guarantee. Shared Redis writers
must be coordinated externally. Management APIs, additional channel protocols,
distributed execution and retention scheduling remain outside C3.

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

C2 tests use fixed clocks, local HTTP/TLS servers and injected RoundTrippers,
never real external notification services. They cover payloads, stable identity,
an independent signing vector, result classification, Retry-After, cancellation,
TLS validation, size limits, resource closure and safe errors. Test the new
package with the race detector when changing its shared client behavior.

C3 adds offline channel/routing validation, real local probe-to-Webhook delivery
with both backends, deterministic worker scans/backoff/exhaustion, empty/resumed
pages, uncertain-outcome reconciliation, missing channels, restart, per-bot limits
and cancellation/close ordering. Redis tests include a forced recovery between
watched reads; empty active hashes may be absent (`TTL == -2`), but existing
incident/outbox hashes must report no TTL (`-1`). Real integration is required
to accept Redis changes; a skip is not a pass.
