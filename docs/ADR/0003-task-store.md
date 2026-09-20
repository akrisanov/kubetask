# ADR 0003: PostgreSQL task store

- Status: Draft
- Date: 2026-09-20
- Depends on: [ADR 0001](0001-authoritative-task-state.md) and [ADR 0002](0002-task-lifecycle.md)

## Context

The accepted ADRs define task authority and lifecycle.
This ADR defines storage records, atomic operations, and schema upgrades before persistence is implemented.
The Go model is a domain projection, not a database schema.

## Records

Use one PostgreSQL schema per deployment. Store queryable facts in typed columns, not a serialized `task.Task`.
Payload contents remain in S3.

| Record | Key and contents |
| --- | --- |
| `tasks` | Application-generated UUID primary key; state, task version, source and specification references and protocol versions, acceptance and state timestamps, requested timeout and effective limits, phase deadlines, cancellation time and version, opaque allocation reference, authorization time and version, authorized boot identity, terminal outcome, retention deadlines, and reconciliation retry schedule |
| `task_executions` | Task ID primary and foreign key; provider, runtime protocol version, deterministic allocation name, resource kinds and namespace, ownership markers, and observed claim, Sandbox, and Pod UIDs. The provider, namespace, resource kind, and allocation name tuple is unique. Identity fields are filled once, never replaced with another allocation |
| `task_history` | Primary key `(task_id, task_version)` and task foreign key; event kind, database timestamp, previous and next state, and bounded reason code. Append facts and transitions, not payloads or complete task snapshots |
| `task_receipts` | Task ID primary and foreign key; immutable execution reference, boot identity, manifest object reference and SHA-256, acceptance time and task version. Validation status, retry deadline, attempt count, next attempt, and bounded failure code are mutable under the task version |
| `idempotency_bindings` | SHA-256 key digest primary key; canonicalization version, request digest, and unique task foreign key. Expiry comes from the task's metadata deadline; no raw key or independent retention period |
| `admission` | One row with a fixed key, used only as the deployment admission lock. Counts come from records, not stored counters |
| `cleanup_tombstones` | Original task ID primary key, with no task foreign key; remaining owned object references, provider identities, unresolved operation intent, capacity reservation, cleanup version, stop evidence, retry schedule, and bounded errors |

Capacity and cleanup are a separately versioned group of columns on `tasks`:
`cleanup_version`, `capacity_held`, allocation-resolution and process-stop facts,
provider-teardown evidence, release time, payload-expiration marker, cleanup
completion markers, remaining references, and retry state. These columns move
to the tombstone at metadata deletion. There is no separate capacity table.

Use positive `bigint` versions, `timestamptz` timestamps, explicit integer
microsecond durations, and 32-byte `bytea` digests. Reject Go integer overflow
and normalize timestamps and durations to database precision before decisions.
Use nullable columns for unknown facts. State checks, nonnegative sizes, and
terminal-state/outcome consistency belong in database constraints. Transition
rules remain in the domain code, not triggers.

Object references include location, key, size, checksum, and version or ETag.
Bounded, versioned JSONB is limited to client metadata, effective resource limits,
and object-reference collections; it never contains source, logs, credentials,
presigned URLs, or raw provider objects. A terminal write records retention
deadlines from the applicable operator policy in the same transaction.

Index due nonterminal tasks by `(next_reconcile_at, id)`, held reservations in
both owning tables, pending tasks, retention deadlines, and due cleanup work.
Foreign keys retain execution, receipt, history, and binding records until task
deletion; tombstones survive it. Public reads enforce logical expiration even
when physical deletion is delayed.

## Transactions and ordering

Use short `READ COMMITTED` transactions and explicit row locks. Retain ADR 0001's
dedicated session advisory lock; it is not a substitute for row-level checks.
External calls never run inside a transaction.
Read and write authoritative facts on the primary. Require `fsync = on` and
`synchronous_commit = on` for task-store transactions; acknowledged commits must
reach durable WAL. Replica failover and backup loss remain subject to ADR 0001's
declared recovery point objective.

Lock order is `admission`, then task rows in task-ID order, then their child rows,
then tombstones in task-ID order. Operations that cannot change pending or active
counts may omit `admission`. Never acquire it after a task lock; restart the
transaction if the required lock set changes. All receipt and execution writers
lock the parent task first, including when the child row does not yet exist.

After acquiring the required locks, read current facts and sample
`clock_timestamp()` once in a separate statement. Use that value for the decision,
history, and derived deadlines. Do not use transaction-start `now()`, client
timestamps, or a sample taken before a lock wait. Count admission usage after
the admission lock is acquired, using a fresh statement snapshot.

| Operation | Atomic work |
| --- | --- |
| Accept | Recheck the idempotency binding, enforce pending capacity, insert task, optional binding, and initial history. Payload staging has already finished outside the transaction |
| Dispatch | Lock admission and task, enforce active capacity, record allocation intent, reserve capacity, and append the transition before any provider create |
| Authorize or accept receipt | Lock task, check current identity, state, version, cancellation, and deadline; persist all related facts and history together |
| Release capacity | Lock admission and owner, verify stop and allocation evidence, clear the reservation, and advance cleanup version without changing outcome |
| Expire metadata | Lock admission and task, copy remaining cleanup and reservation into a tombstone, then delete task and child records. The reservation stays counted exactly once |

Admission counts `Pending` task rows and the sum of held reservations in tasks
and tombstones. Moving a pending task to any other state also takes the admission
lock. Terminal state alone never clears a reservation. An expired idempotency
binding can be removed under this lock to allow key reuse without discarding
the old task's outstanding cleanup. Check a retained binding before admission
and external input checks, including the final acceptance recheck.

### Compare and update

A lifecycle mutation updates only its owned columns using
`WHERE id = $id AND state = $expected_state AND task_version = $expected_version`.
It advances the version once and appends the new history entry in the same
transaction. Zero affected rows means stale or missing state: roll back all
related writes and return a conflict, not success. Never replay a stale decision
against a newer row; reload and reevaluate it with a new database time.

Receipt validation uses the same task version and cannot overwrite cancellation
or an outcome chosen concurrently. Terminal outcome columns are never updated
again. Read an aggregate in one statement or while holding its parent lock so
task and receipt facts cannot come from different revisions.

Cleanup updates compare expected state and `cleanup_version` on a task, or
`cleanup_version` on a tombstone. They change neither task version nor terminal
outcome. Dispatch initializes reservation fields and advances both versions.
The current Go capacity methods and history slice do not define this storage
layout; persistence integration must map these groups explicitly and append
only new history entries.

Documented no-ops, such as repeated cancellation or an identical receipt, return
the current record without incrementing a version or appending history. They
are checked against current retained data, not an old caller snapshot.
An expiry retry that finds only the tombstone resumes its cleanup; it never
creates another reservation or reconstructs the deleted public task.

### Authorization and receipts

Authorization requires `Allocating`, no prior authorization, the exact execution
identity, and ADR 0002's cancellation and deadline guards. Commit authorization,
boot identity, execution deadline, version, and history before returning a new
authorization result. Only the caller that performed that acknowledged commit
may send the one-use start response. A domain `AuthorizationRecorded` flag or
SQL `RETURNING` row is not commit confirmation.

If commit or response delivery is uncertain, send no further permit. A later
read can discover authorization but cannot recreate the winning response, even
for the same boot identity. Do not automatically retry this operation across an
ambiguous commit. Safe database retries require a known rollback and must rerun
the guards with a fresh time.

For receipts, first check retention and existing receipt under the task lock.
The same execution identity, boot identity, immutable manifest reference, and
digest return the original receipt, even after cancellation, the deadline, or
terminal completion. Different contents return a conflict; never use an upsert
that replaces receipt fields. With no receipt, check expected state and version
and all eligibility guards before inserting it, advancing the task version,
appending history, and setting validation's deadline to acceptance plus 60 seconds.
A unique task key prevents a second receipt. Unlike a start permit, a committed
receipt can safely answer a retry after a lost commit acknowledgment.

## Migrations and restore

Keep numbered, immutable SQL migrations and a `schema_migrations` ledger with
version, checksum, and application time. Each v0.1 migration and its ledger
entry commit in one transaction. Nontransactional DDL requires a later design.
Run migrations explicitly using a separate DDL role after stopping API writers
and reconciliation and acquiring the same deployment advisory lock.

The binary declares its supported PostgreSQL versions and exact schema version.
Readiness fails for an unsupported server, incompatible schema, or inconsistent
migration ledger and checksums. There are no automatic
startup migrations, mixed-version writers, or automatic down migrations in
v0.1. Migration failure leaves the previous version intact. After a successful
incompatible migration, roll back with a compatible binary or a verified backup,
not by editing the migration ledger.

Upgrades and backups must preserve task and cleanup versions, identities,
authorization, receipt timing, deadlines, and held reservations. Immutable S3
specifications and manifests keep their protocol versions; a database migration
cannot rewrite them. New binaries must read retained versions or reject the
upgrade before migration.

Restore the schema and ledger together, use a compatible binary, and complete
ADR 0001's external-resource audit before readiness. A restore does not replay
history, renew deadlines, clear capacity, or issue start permits. Missing
authorization or receipt evidence cannot be inferred from a manifest alone.
Backup compatibility and restore tests must include tasks at each lifecycle
stage and tombstones with outstanding reservations.

## Consequences and verification

Typed records and two version groups require an explicit domain mapping, but
avoid whole-row overwrites, event replay, and a separate queue or counter service.
The admission lock serializes capacity changes intentionally for v0.1.

Before persistence ships, use real PostgreSQL tests for competing admissions,
stale updates with history rollback, authorization commit uncertainty, identical
and conflicting receipts, deadlines crossed while waiting on locks, atomic
tombstone transfer, and migration and restore failure. In-memory domain tests
cannot establish these guarantees.

PostgreSQL references: [transaction isolation](https://www.postgresql.org/docs/current/transaction-iso.html),
[locking](https://www.postgresql.org/docs/current/explicit-locking.html),
[timestamp functions](https://www.postgresql.org/docs/current/functions-datetime.html), and
[commit durability](https://www.postgresql.org/docs/current/runtime-config-wal.html).
