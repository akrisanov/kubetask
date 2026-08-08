# ADR 0001: Use PostgreSQL for authoritative task state

- Status: Accepted
- Date: 2026-08-08

## Context

KubeTask presents execution as a durable asynchronous task lifecycle. An
accepted task must survive control-plane restarts, remain safe when API and
reconciliation activity overlap, and retain its result independently of the
execution workload.

Authoritative task state must support:

- immutable internal task identities
- deterministic idempotent submission
- bounded pending and active task admission
- atomic state transitions that reject stale writers
- durable cancellation intent
- reconciliation after partial failure or process restart
- execution state that is independent of Agent Sandbox resource state
- cleanup state that is independent of execution outcome
- retention and expiration of task metadata

Task source, input files, captured output, result manifests, and artifacts can
be substantially larger than task metadata. They require different access,
retention, and transfer behavior from lifecycle state.

KubeTask v0.1 is single-tenant. Its default limits are 100 pending tasks and 20
active tasks. Multiple control-plane replicas are desirable, but they are not a
MUST requirement for v0.1.

## Decision

PostgreSQL is the authoritative store for KubeTask task metadata, idempotency,
task lifecycle state, transition history, reconciliation scheduling, and
cleanup progress.

S3-compatible object storage is the authoritative store for task
specifications, source, inputs, captured stdout and stderr, result manifests,
and artifacts. PostgreSQL stores immutable references and validation metadata
for those objects, not their contents.

Kubernetes and Agent Sandbox resources are observed execution state. They are
not authoritative task records. Deleting or replacing an execution resource
must not erase an accepted task or revise its terminal result.

KubeTask will not use a Task custom resource, SQLite, object storage, or
process-local memory as an alternative authority. It will not add a message
broker, event-sourced task model, or transactional outbox in v0.1.

## v0.1 deployment and coordination model

KubeTask v0.1 supports one control-plane replica and one active reconciler.
This deliberately defers NFR-026, which recommends multiple replicas, to avoid
introducing distributed work claims before there is evidence that they are
needed.

The process holds a deployment-scoped PostgreSQL advisory lock on a dedicated
connection while it is ready and reconciling. This prevents overlapping Pods
during rollout or operator error from running two reconcilers. Loss of the lock
or its database connection stops reconciliation and removes readiness before
the process attempts to acquire the lock again. This singleton lock does not
claim individual tasks.

The v0.1 Deployment uses the `Recreate` strategy so the current lock holder is
terminated before its replacement must become ready. This causes brief API and
reconciliation downtime during rollout but does not terminate Agent Sandbox
workloads. The advisory lock remains a guard against manual scaling and
unexpected Pod overlap.

The single replica still has concurrent API and reconciliation activity. All
task mutations therefore use database transactions and optimistic concurrency
control. A bounded in-process worker pool may reconcile several distinct tasks
at once, but one task cannot be queued to two local workers concurrently.

The reconciler discovers work through an indexed, ordered scan of incomplete
tasks whose `next_reconcile_at` value is due. It does not use `LISTEN/NOTIFY`,
`FOR UPDATE SKIP LOCKED`, or renewable database leases in v0.1. These are
possible later optimizations for multiple active reconcilers and do not form
part of the persistence contract.

A control-plane restart may briefly interrupt the API and reconciliation, but
it does not terminate running Agent Sandbox workloads. After restart, the
reconciler scans incomplete tasks and resumes processing within the recovery
objective defined in the requirements.

## Persistence boundaries

### Task records

The authoritative task record contains at least:

- current task state and a monotonically increasing state version
- creation, update, state-entry, and terminal timestamps
- the requested execution timeout and deadline
- the effective pending, sandbox-allocation, and execution deadlines
- cancellation intent and its timestamp
- a versioned task-specification object reference
- an opaque execution reference
- an irreversible execution-attempt marker
- a versioned result-manifest object reference when published
- terminal outcome and stable public failure code
- reconciliation attempt count, next reconciliation time, and last failure code
- metadata and payload expiration timestamps
- cleanup state, attempt count, next attempt time, and last failure code

The task domain treats the execution reference as an opaque KubeTask identity.
The persistence adapter binds it to a provider-specific record containing the
provider, Kubernetes API group and resource, namespace, name, and UID. The
domain model does not expose Agent Sandbox, Pod, HTTP, MCP, or Kubernetes DTOs.

Persisted error fields contain bounded stable codes and redacted diagnostic
text. They do not contain credentials, stack traces, source, user output, raw
SDK responses, or serialized Kubernetes objects.

PostgreSQL time is authoritative for task timestamps, deadlines, retry
schedules, and retention. Infrastructure timestamps may be recorded as
observations but do not override authoritative task time.

### State transitions

Every task mutation runs in a transaction. A state update includes the expected
state and version in its predicate and increments the version on success. A
stale or invalid transition changes no rows and becomes an idempotent no-op or
a conflict according to the application operation.

Task transition rules live in one domain package shared by API commands and the
reconciler. SQL constraints protect state-independent invariants. SQL triggers
do not implement the state machine.

A successful state transition appends a compact transition record in the same
transaction. The record contains the task ID, previous and new states,
database timestamp, stable reason code, and bounded correlation data.

Terminal execution outcomes are immutable. Cleanup has its own state and
version so cleanup failure cannot replace, revise, or move a terminal execution
outcome backward.

## Submission and idempotency

### Idempotency semantics

An idempotency key is optional. In single-tenant v0.1, its scope is the KubeTask
deployment. KubeTask stores a SHA-256 digest of the key and does not persist or
log the raw value.

The request digest is a SHA-256 digest calculated from a versioned canonical
representation of the client-supplied semantic request. It preserves
distinctions such as a missing value versus an explicit zero and is calculated
before operator defaults are applied. Map ordering and transport-specific
encoding do not affect the digest.

When an idempotency key is present, KubeTask checks it before admission and
before revalidating external input objects:

- the same key and request digest return the existing task in its current state
- the same key and a different request digest return a deterministic conflict
- a new key proceeds through validation and admission

This order ensures that a retry returns its accepted task even when current
capacity, configuration, or input-object availability has changed.

Idempotency records have the same retention period as task metadata. After a
task and its idempotency record expire, the key may be used for a new task. A
future authenticated multi-tenant version must add a service-derived tenant
scope rather than use client metadata as the scope.

The task row may hold the nullable idempotency-key digest, request digest, and
digest-format version. A partial unique index is sufficient for v0.1. A
separate idempotency table is required only if its retention later differs from
task retention.

### Submission rejection

Validation, policy, unsupported-behavior, and saturation failures are
synchronous submission errors. They return stable public error codes and
create no task record, idempotency binding, or execution resource. Rejections
are represented in operational logs and metrics with bounded correlation data.

Only a request admitted into the pending queue receives an internal task ID and
enters the task lifecycle. Reusing an idempotency key after a rejected
submission re-evaluates the request because no accepted task is bound to that
key.

### Durable acceptance protocol

PostgreSQL and object storage cannot participate in one transaction. KubeTask
uses the following ordered protocol for a valid request that may be admitted:

1. Parse and bound the request sufficiently to canonicalize it and resolve any
   existing idempotency record
2. Validate the complete request and referenced input objects
3. Generate the immutable internal task ID and effective task specification
4. Upload the source and specification to unique, immutable task object keys
5. Start a database transaction and resolve any concurrent idempotency winner
6. Enforce admission and, if admitted, insert the task, idempotency data, and
   first transition atomically
7. Commit before returning the task ID

An object upload does not constitute durable task acceptance. If admission
rejects the request, the database transaction fails, or a concurrent request
wins the idempotency race, uploaded objects that are not referenced by the
accepted task are orphans. Cleanup removes them after a safety interval. Object
cleanup is never part of the submission request's database transaction.

An admitted task cannot reference a source or specification that was not
successfully uploaded and verified. A database failure after commit but before
the response is handled through the idempotency key when provided. Without an
idempotency key, the client cannot resolve that ambiguity through the public
task API and a retry may create another task. Clients that require unambiguous
submission retries must provide an idempotency key.

## Admission

Admission uses one deployment-scoped database row as a transaction lock. After
locking it, KubeTask derives pending and active counts from indexed
authoritative task rows. It does not maintain counters that require a separate
repair protocol in v0.1.

Submission transactions lock this row before adding pending work. Transitions
that move tasks into or out of pending or active categories use the same lock.
This serializes the small admission-critical section while external storage and
Kubernetes operations remain outside the transaction.

The pending limit is enforced when admitting a valid submission. The active
limit is enforced when dispatching pending work. A refused submission returns
a saturation error and does not reserve pending or active capacity.

## Reconciliation and execution resources

A reconciler does not hold a database transaction while calling external
systems such as Kubernetes, Agent Sandbox, or object storage. It reads a task,
performs one bounded external step, and persists the observation with an
expected state and version. Transient failures update persisted retry state
with bounded exponential backoff and jitter. Permanent failures and exhausted
retries become a task failure or operator-visible cleanup failure.

The expected Agent Sandbox claim name is derived deterministically from the
internal task ID and recorded before creation is attempted. After an ambiguous
create response, reconciliation reads that exact name before attempting
another create. KubeTask adopts it only when bounded ownership metadata matches
the task.

Kubernetes names can be reused after deletion, so KubeTask records and validates
the resource UID after creation. An object with the expected name but an
unexpected UID or ownership marker is an infrastructure conflict and is never
silently adopted.

Allocation and preparation steps that may be retried must be incapable of
starting user code. Before KubeTask invokes a runtime start operation or
releases a start gate, it commits an irreversible execution authorization and
the exact execution resource UID in PostgreSQL. The start action may target
only that recorded resource.

If creating the execution resource itself can start user code, KubeTask commits
the irreversible marker and deterministic resource identity before the create
request. After an ambiguous create, it may observe that identity but may not
create a replacement. A start gate is preferred because it permits allocation
and preparation retries without risking a second user-code execution.

After execution is authorized, KubeTask never creates a replacement execution
environment or starts user code again for that task. If observed state cannot
prove the outcome after an ambiguous failure, the task ends as an
infrastructure failure. The lifecycle and runtime protocol define the exact
start-gate handshake without weakening this persistence rule.

## Object references and result finalization

Every KubeTask-owned object reference contains the configured bucket or storage
location, key, byte size, SHA-256 checksum, and object version or ETag when
available. A validated input reference contains its key, workspace path,
observed size, and a stable object identity. The identity is a version ID when
available and otherwise an ETag that the store can enforce through a
conditional read. KubeTask rejects the input if the configured store cannot
provide and enforce either identity. Presigned URLs are generated on demand and
are never persisted.

Task-owned source, specification, log, manifest, and artifact keys are unique
and treated as immutable. A supported S3-compatible store must provide:

- atomic visibility of a completed single-object write
- read-after-write behavior sufficient for bounded verification and retry
- conditional create or an equivalent versioned-write mechanism for
  KubeTask-owned objects
- version-addressed reads or conditional reads that fail when an accepted input
  no longer has its recorded identity
- streamed or multipart transfer for bounded gateway memory usage
- paginated prefix listing with last-modified metadata and idempotent deletion
  for KubeTask-owned keys

The operator configures object-store lifecycle cleanup for abandoned multipart
uploads. KubeTask fails readiness if the configured storage cannot meet the
required identity and conditional-operation semantics.

The runtime uses a version-addressed GET or a conditional GET with the identity
recorded during admission and fails rather than consume a changed object. It
uploads logs and artifacts before publishing the versioned result manifest as
the final commit marker. A retry may confirm an existing byte-identical
manifest, but it cannot replace it with different content. Conflicting
manifests become an infrastructure failure.

The reconciler validates the manifest and commits its object reference and the
terminal execution outcome in one PostgreSQL transaction. Workload completion
alone never implies task success. A controlled outcome without a valid manifest
becomes an infrastructure failure according to the lifecycle rules.

PostgreSQL and object storage do not have cross-store transactions. Correctness
therefore relies on immutable objects, idempotent publication, authoritative
database references, and reconciliation of unreferenced or missing objects.

## Retention and cleanup

Task metadata and payloads have independent expiration timestamps derived from
the terminal timestamp and operator configuration.

Payload cleanup uses this order:

1. Mark retained payload references expired in PostgreSQL and commit
2. Delete the corresponding objects
3. Mark payload cleanup complete

After step 1, APIs do not return stored references or issue new presigned URLs.
A crash can leave hidden objects that cleanup safely retries. Object deletion
never removes or changes the terminal task outcome.

Task metadata and its idempotency binding are deleted at the configured metadata
retention deadline. When payload or execution-resource cleanup is still
incomplete, the same transaction first creates a compact operator-only cleanup
tombstone. The tombstone contains only the task ID, remaining KubeTask-owned
object and execution references, cleanup state and version, retry timing,
attempt count, and bounded failure code. It contains no client metadata,
request digest, source, result, or public task state.

Cleanup tombstones remain authoritative cleanup work until deletion succeeds.
They are retried and exposed through operator signals but are not returned by
the task API and do not extend idempotency-key retention. A completed tombstone
is deleted. Orphan detection treats references held by cleanup tombstones as
live and also removes task-owned objects with no task or tombstone reference
after a safety interval.

Retention and orphan cleanup delete only keys in KubeTask-owned task prefixes.
Caller-owned input references expire from task metadata but KubeTask never
deletes the referenced input objects.

## Deployment, migration, and recovery

Production deployments supply PostgreSQL as an external platform dependency.
KubeTask does not install or operate a production PostgreSQL cluster. Connection
credentials come from a Kubernetes Secret and follow the platform's transport
security, rotation, backup, monitoring, and least-privilege policies.

The local Kind environment may deploy a disposable PostgreSQL instance.
Integration tests that exercise persistence and reconciliation run against a
supported PostgreSQL version. SQLite is not used as a semantic substitute.

Database changes use ordered, versioned migrations. Because v0.1 has one
control-plane replica, deployments use a stop, migrate, start sequence. The
service refuses readiness when the schema is incompatible. Online mixed-version
migrations are deferred until multiple control-plane replicas are supported.

The v0.1 recovery contract is:

- a normal process or Pod restart loses no committed task state
- the operator defines and documents PostgreSQL recovery point and recovery
  time objectives before a production deployment
- PostgreSQL backup and point-in-time recovery are external platform
  responsibilities
- KubeTask does not claim zero data loss beyond the recovery point provided by
  that PostgreSQL deployment
- the control plane remains stopped while authoritative database state is
  restored
- a restore procedure is tested before the deployment is declared
  production-ready
- a restored task row is the authoritative starting state, and reconciliation
  applies infrastructure and object-store observations only through valid
  forward transitions
- after dependency recovery, all incomplete tasks are reconsidered within the
  60-second recovery objective
- missing source or specification data for a non-terminal task becomes an
  infrastructure failure
- missing retained result data is reported as unavailable or expired without
  revising the terminal execution outcome
- unreferenced objects left by an earlier database recovery point are cleaned
  as orphans after a safety interval
- the restoration procedure lists KubeTask-owned Agent Sandbox resources and
  compares their task IDs, ownership markers, and UIDs with restored task state
- a resource with no restored task is an orphan and is stopped or released
- when a matching task exists but its restored execution reference or
  authorization marker does not account for the resource, KubeTask first
  finalizes a valid immutable result manifest when one exists and otherwise
  records an irreversible execution attempt and infrastructure failure
- a resource found during recovery is never used to start or repeat user code
- all orphaned or uncertain execution resources are stopped or released before
  the service resumes task acceptance
- idempotency and at-most-once execution cannot be guaranteed for task records
  lost beyond the PostgreSQL deployment's recovery point

Readiness reports not ready when PostgreSQL is unavailable, its schema is
incompatible, object storage cannot support safe task acceptance, or a
post-restore orphan audit has not completed. Readiness also requires the
singleton reconciler lock. Liveness does not depend on PostgreSQL, object
storage, Agent Sandbox, or the success of an individual task.

## Alternatives considered

### KubeTask Task custom resource

A custom resource would provide Kubernetes persistence, watches, and
`resourceVersion` concurrency without adding a database. It is not selected
because KubeTask exposes an imperative operation API, requires idempotency and
atomic admission across records, and retains application task history that is
independent of cluster workload resources. It would also couple task recovery
and retention to the Kubernetes API and etcd backup policy.

### SQLite on a persistent volume

SQLite could meet many v0.1 behaviors with one replica. It is not selected
because production recovery would depend on volume attachment and filesystem
semantics, and a later move to multiple replicas would require a second
persistence implementation. PostgreSQL is used in local integration tests so
development and production exercise the same concurrency behavior.

### Object storage as authoritative task state

Object storage is durable and already required for payloads. It does not offer
the multi-record transactions, uniqueness constraints, indexed work discovery,
or coordination semantics required by idempotency, admission, and task state.
Using it as the sole authority would move database behavior into KubeTask.

### Kubernetes or Agent Sandbox resource state

SandboxClaim, Sandbox, and Pod status describe execution infrastructure, not
durable acceptance, idempotency, cancellation intent, result retention, or
independent cleanup progress. Making them authoritative would also blur the
boundary between KubeTask reconciliation and Agent Sandbox reconciliation.

### In-memory state

Process-local state cannot provide restart-safe acceptance, cancellation,
idempotency, or reconciliation and is rejected for all environments that
exercise the task lifecycle.

## Consequences

### Positive

- Task acceptance, idempotency, transitions, admission, and history share
  transactional invariants
- Task metadata survives control-plane and execution-resource loss
- Large or sensitive payloads remain outside PostgreSQL and Kubernetes etcd
- Reconciliation can query overdue and incomplete work directly
- Agent Sandbox remains an execution provider rather than a second task
  authority
- The v0.1 implementation avoids distributed worker claims and a separate
  queueing system

### Negative

- PostgreSQL is a required production and readiness dependency
- Operators must provide database security, backup, restore, monitoring, and
  capacity management
- v0.1 has brief API and reconciliation downtime during rollout or replica
  failure
- PostgreSQL and object storage require explicit cross-store recovery behavior
- Ambiguous external operations still require deterministic identities,
  irreversible markers, and conservative failure handling
- Multiple active control-plane replicas require a later design change
