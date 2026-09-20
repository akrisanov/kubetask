# ADR 0001: Use PostgreSQL for authoritative task state

- Status: Accepted
- Date: 2026-08-08
- Amended by: [ADR 0002](0002-task-lifecycle.md), 2026-09-20

## Context

KubeTask presents execution as a durable asynchronous task lifecycle. Accepted
tasks must survive control-plane restarts, support idempotent submission and
cancellation, remain safe under concurrent API and reconciliation activity,
and retain results independently of execution workloads.

Task metadata requires transactions, uniqueness, concurrency control, indexed
work discovery, and retention queries. Task source, inputs, logs, manifests,
and artifacts have different size, access, and transfer requirements. Agent
Sandbox owns sandbox resource lifecycle, while KubeTask owns task lifecycle.

KubeTask v0.1 is single-tenant, with default limits of 100 pending and 20 active
tasks. Multiple control-plane replicas are desirable but are not required for
v0.1.

## Decision

PostgreSQL is authoritative for:

- task metadata and current lifecycle state
- idempotency bindings
- transition history
- reconciliation scheduling and retry state
- cleanup progress

S3-compatible object storage is authoritative for task specifications, source,
inputs, captured stdout and stderr, result manifests, and artifacts. PostgreSQL
stores immutable object references and validation metadata, not payload
contents.

Kubernetes and Agent Sandbox resources are observed execution state, not task
records. Their deletion or replacement cannot erase an accepted task or revise
its terminal result.

KubeTask will not use a Task custom resource, SQLite, object storage, or memory
as an alternative authority. v0.1 will not add a message broker, event-sourced
task model, or transactional outbox.

## v0.1 coordination model

v0.1 runs one control-plane replica and one active reconciler. This
intentionally defers NFR-026 to avoid distributed task claims before capacity
testing demonstrates a need.

The process holds a deployment-scoped PostgreSQL advisory lock on a dedicated
connection while ready and reconciling. Loss of the lock stops reconciliation
and removes readiness. The Deployment uses the `Recreate` strategy so a rollout
terminates the lock holder before its replacement must become ready. This
causes brief API downtime but does not terminate Agent Sandbox workloads.

The reconciler uses indexed scans of due incomplete tasks and a bounded local
worker pool with per-task deduplication. It does not use `LISTEN/NOTIFY`,
`FOR UPDATE SKIP LOCKED`, or per-task database leases in v0.1.

## Persistence guarantees

The task record includes the current state and version, requested and effective
deadlines, cancellation intent, object references, an opaque execution
reference, completion receipt, terminal outcome, reconciliation timing,
retention timestamps, and cleanup state. Provider-specific Kubernetes identity is stored outside the
domain model and includes the API resource, namespace, deterministic name, UID,
and ownership markers.

PostgreSQL time is authoritative for acceptance, state timestamps, deadlines,
retry schedules, and retention. Persisted errors use bounded stable codes and
redacted diagnostic text. Source, output, credentials, stack traces, raw SDK
responses, and Kubernetes objects are not stored as diagnostic fields.

Every task mutation runs in a transaction and includes the expected state and
version. Stale or invalid updates change no rows. Successful state transitions
append a compact history entry in the same transaction. Transition rules live
in the shared task domain package rather than SQL triggers.

Terminal execution outcomes are immutable. Cleanup has separate state and
versioning so cleanup failure cannot replace or move an execution outcome
backward.

## Submission and idempotency

An idempotency key is optional and deployment-scoped in single-tenant v0.1.
KubeTask stores its SHA-256 digest, not the raw value. The request digest is
calculated from a versioned canonical client request before operator defaults
are applied. Canonicalization preserves missing values versus explicit zeros
and ignores transport encoding and map ordering.

KubeTask resolves an existing idempotency binding before current admission or
external input checks. The same key and request digest return the existing task.
The same key with a different digest returns a deterministic conflict. The
binding expires with task metadata, after which the key may be reused.

Validation, policy, unsupported-behavior, and saturation failures are
synchronous pre-task errors. They create no task, idempotency binding, or
execution resource. Only an admitted request receives a task ID. Reusing a key
after rejection re-evaluates the request.

PostgreSQL and object storage cannot commit atomically. Durable acceptance uses
this order:

1. Parse and bound the request, then resolve existing idempotency
2. Validate the complete request and input objects
3. Generate the internal task ID and effective specification
4. Upload source and specification to unique immutable object keys
5. In one database transaction, resolve a concurrent idempotency winner,
   enforce admission, and insert the task, binding, and first transition
6. Commit before returning the task ID

The database commit is the acceptance point. Failed admission, a failed
transaction, or a lost idempotency race may leave unreferenced objects. Cleanup
removes them after a safety interval. Without an idempotency key, a lost response
after commit is ambiguous and a retry may create another task.

## Admission

Admission uses one deployment-scoped database row as a transaction lock.
Pending counts come from indexed task rows. Active counts include unreleased
reservations in task rows and cleanup tombstones, as defined by ADR 0002.
Transactions that change admission counts or transfer a reservation to a
tombstone use the same lock.

The pending limit is enforced during submission, and the active limit is
enforced during dispatch. External storage and Kubernetes operations never run
inside the admission transaction. v0.1 does not maintain separate counters or
a counter-repair protocol.

## Reconciliation and execution safety

The reconciler never holds a database transaction while calling Kubernetes,
Agent Sandbox, or object storage. It performs one bounded external step and
persists the observation with optimistic concurrency. Retry timing and bounded
exponential backoff with jitter are durable. Permanent failures and exhausted
retries become task failures or operator-visible cleanup failures.

The Agent Sandbox claim name is derived from the internal task ID and recorded
before creation. After an ambiguous create, KubeTask reads that exact name and
validates ownership. It records the Kubernetes UID and never silently adopts a
same-named resource with a different UID or ownership marker.

Allocation creates a runtime that cannot yet execute code. ADR 0002 requires
one-use execution authorization, bound to the exact resource UID and runtime
boot identity, before source retrieval or preparation begins. The authorization
commits before the runtime receives its start permit.

After authorization, KubeTask never creates a replacement environment or starts
user code again for that task. An ambiguous outcome becomes an infrastructure
failure. Lost authorization replies and runtime restarts never receive another
permit. ADR 0002 also prohibits replacing a lost allocation before authorization.

## Object storage and results

KubeTask-owned object references include the storage location, key, byte size,
SHA-256 checksum, and version or ETag when available. Accepted inputs require a
stable version ID or an ETag enforceable through a conditional read. The runtime
uses a version-addressed or conditional GET and fails rather than consume a
changed input.

The supported S3-compatible profile must provide atomic completed writes,
read-after-write behavior, conditional or versioned creation, conditional or
versioned reads, streaming or multipart transfer, and paginated listing and
deletion of KubeTask-owned keys. The operator configures cleanup for abandoned
multipart uploads. Presigned URLs are generated on demand and never persisted.

The runtime uploads logs and artifacts before publishing the versioned result
manifest as the payload commit marker. An existing byte-identical manifest may
be confirmed but never replaced with different content. ADR 0002 additionally
requires a completion receipt in PostgreSQL before cancellation and the execution
deadline. The reconciler validates that receipt's manifest and commits the
terminal outcome. Publication or workload completion alone never implies success.

PostgreSQL and object storage have no cross-store transaction. Correctness
depends on immutable objects, idempotent publication, authoritative database
references, and reconciliation of missing or unreferenced objects.

## Retention and recovery

Payload cleanup first marks references expired in PostgreSQL, then deletes the
objects, then records completion. APIs stop returning references and issuing
presigned URLs after the first committed step. Cleanup deletes only
KubeTask-owned keys and never deletes caller-owned input objects.

Task metadata and its idempotency binding are deleted at the metadata-retention
deadline. If payload or execution-resource cleanup remains incomplete, the same
transaction creates an operator-only cleanup tombstone containing only the
remaining references, any unreleased capacity reservation, retry state, and
bounded failure data. Reservation transfer holds the admission lock and preserves
the active count. Tombstones remain until cleanup succeeds and do not extend
public task or idempotency retention.

Production PostgreSQL is supplied and operated by the deployment platform.
KubeTask does not install a production database. Kind may include a disposable
instance, and integration tests use PostgreSQL rather than SQLite. Database
changes use versioned migrations and a stop, migrate, start sequence in v0.1.

Normal process or Pod restart loses no committed task state. Production
deployments must define and restore-test PostgreSQL recovery point and recovery
time objectives. KubeTask does not claim zero data loss, idempotency, or
at-most-once execution for task records lost beyond that recovery point.

After a database restore, KubeTask audits owned object and Agent Sandbox
resources before accepting work. Unreferenced resources are cleaned up. If a
resource matches a restored task but its execution identity, authorization, or
timely completion receipt was lost, a manifest alone cannot establish success.
KubeTask finalizes only when retained evidence satisfies ADR 0002; otherwise it
records an infrastructure failure. It never restarts uncertain work.

Readiness requires PostgreSQL, a compatible schema, the singleton lock, safe
object-storage capabilities, and completion of any post-restore audit. Liveness
does not depend on downstream availability or individual task success.

## Alternatives considered

### KubeTask Task custom resource

A custom resource would provide Kubernetes persistence, watches, and
`resourceVersion` concurrency. It was rejected because KubeTask exposes an
imperative task API, requires atomic idempotency and admission across records,
and retains application state independently of cluster workloads. It would also
couple task recovery and retention to Kubernetes API and etcd operations.

### SQLite on a persistent volume

SQLite could support a single-replica v0.1. It was rejected because recovery
would depend on volume attachment and filesystem semantics, and later
multi-replica support would require another persistence implementation.

### Object storage or infrastructure state

Object storage lacks the required transactions, uniqueness, and indexed work
discovery. SandboxClaim, Sandbox, and Pod status do not represent durable
acceptance, idempotency, cancellation intent, result retention, or cleanup.
In-memory state cannot survive restart. None is suitable as task authority.

## Consequences

### Positive

- Task acceptance, idempotency, transitions, admission, and history share
  transactional invariants
- Task state and results outlive control-plane and workload processes
- Payloads remain outside PostgreSQL and Kubernetes etcd
- Agent Sandbox remains an execution provider rather than a second task
  authority
- v0.1 avoids distributed task claims and a separate queueing system

### Negative

- PostgreSQL is a required production and readiness dependency
- Operators must provide database security, backup, restore, monitoring, and
  capacity management
- v0.1 has brief API downtime during rollout or replica failure
- Cross-store and external API failures require explicit reconciliation
- Multiple active control-plane replicas require a later design

## Required follow-up designs

ADR 0002 defines the task lifecycle. Implementation still requires:

1. Versioned task-specification and result-manifest contracts, including
   canonical request and object-store profiles
2. A PostgreSQL schema and migration design
3. A reconciliation and cleanup design
4. A recovery runbook with concrete recovery objectives
