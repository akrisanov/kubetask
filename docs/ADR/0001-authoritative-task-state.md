# ADR 0001: Use PostgreSQL for authoritative task state

- Status: Accepted
- Date: 2026-08-08
- Amended by: [ADR 0002](0002-task-lifecycle.md), 2026-09-20

## Context

Accepted tasks must survive control-plane restarts, support idempotent submission
and cancellation, remain safe under concurrent updates, and retain results after
workloads stop. Metadata needs transactions, uniqueness, indexed work discovery,
and retention queries; payloads need separate storage and transfer.

v0.1 is single-tenant, with default limits of 100 pending and 20 active tasks.
Multiple control-plane replicas are desirable but not required.

## Decision

| System | Responsibility |
| --- | --- |
| PostgreSQL | Authoritative task metadata, lifecycle state, idempotency bindings, history, reconciliation scheduling, retries, and cleanup progress |
| S3-compatible object storage | Authoritative specifications, source, inputs, stdout, stderr, manifests, and artifacts |
| Kubernetes and Agent Sandbox | Observed execution state and sandbox resource lifecycle |

KubeTask owns task lifecycle. PostgreSQL stores immutable object references and
validation metadata, never payload contents. Deleting or replacing execution
resources cannot erase accepted tasks or revise terminal results.

Task custom resources, SQLite, object storage, and memory are not alternative
task authorities. v0.1 adds no broker, event-sourced task model, or transactional
outbox.

## v0.1 coordination model

One control-plane replica runs one active reconciler. This defers NFR-026 and
distributed task claims until capacity testing demonstrates a need.

The process holds a deployment-scoped PostgreSQL advisory lock on a dedicated
connection while ready and reconciling. Detecting lock loss removes readiness
and stops new mutations and dispatch. The lock does not fence transactions on
other connections or external calls already in flight. The Deployment uses
`Recreate` to terminate the previous process before its replacement becomes ready,
briefly interrupting the API but leaving Agent Sandbox workloads running.

Reconciliation uses indexed scans of due incomplete tasks and a bounded worker
pool with per-task deduplication. v0.1 uses no `LISTEN/NOTIFY`,
`FOR UPDATE SKIP LOCKED`, or per-task database leases.

## Persistence guarantees

Task records retain state and version, requested timeout and effective deadlines,
cancellation intent, object references, opaque execution identity, completion
receipt, terminal outcome, reconciliation timing, retention, and cleanup state.
Provider identity stays outside the domain model: API resource, namespace,
deterministic name, UID, and ownership markers.

PostgreSQL time governs acceptance, state timestamps, deadlines, retries, and
retention. Persisted errors contain bounded stable codes and redacted diagnostics,
never source, output, credentials, stack traces, raw SDK responses, or Kubernetes
objects.

Lifecycle mutations check expected state and task version transactionally;
stale or invalid writes change no rows. Transitions append compact history
atomically. Rules live in the shared task domain package, not SQL triggers.
Terminal outcomes are immutable. Cleanup uses its own version and, on task rows,
expected state, as defined in ADR 0003. Documented no-ops return current retained
facts without advancing versions or history.

## Submission and idempotency

Idempotency keys are optional and deployment-scoped. Store their SHA-256 digest,
never the raw key. Hash a versioned canonical client request before applying
operator defaults, preserving missing values versus explicit zeros and ignoring
transport encoding and map order.

Resolve retained bindings before current policy, admission, or external input
checks. Compare retries using the binding's canonicalization version: the same
key and request digest return the existing task; a different digest returns a
deterministic conflict. Upgrades must support retained canonicalization versions
until their bindings expire. Bindings expire with task metadata, permitting key
reuse; retries do not extend retention.

Validation, policy, unsupported-behavior, and saturation failures are synchronous
pre-task errors: no task, binding, or execution resource is created. Only admitted
requests receive task IDs. Retrying a rejected key re-evaluates the request.

PostgreSQL and object storage cannot commit atomically. Accept in this order:

1. Parse and bound the request, then resolve existing idempotency.
2. Validate the complete request and input objects.
3. Generate the internal task ID and effective specification.
4. Upload source and specification to unique immutable object keys.
5. In one database transaction, resolve a concurrent idempotency winner,
   enforce admission, and insert the task, optional binding, and first transition.
6. Commit before returning the task ID.

The commit is the acceptance point. Failed admission, transactions, or idempotency
races can leave unreferenced objects; cleanup removes them after a safety interval.
Without a key, retrying a lost acceptance response may create another task.

## Admission

One deployment-scoped row locks all transactions that change admission counts
or transfer reservations to tombstones. Enforce the pending limit at submission
using indexed task rows, and the active limit at dispatch using unreleased
reservations in tasks and tombstones under ADR 0002. External storage and
Kubernetes calls stay outside admission transactions. v0.1 maintains no separate
counters or counter-repair protocol.

## Reconciliation and execution safety

The reconciler performs one bounded external step outside database transactions,
then persists the observation with optimistic concurrency. Retry timing and
bounded exponential backoff with jitter are durable. Permanent failures and
exhausted retries become task failures or operator-visible cleanup failures.

Derive the Agent Sandbox claim name from the internal task ID and persist it
before creation. Resolve ambiguous creates by reading that name and verifying
ownership. Record its UID; never adopt a resource with a different UID or
ownership marker.

Allocated runtimes cannot execute code until authorization has an acknowledged
commit and a one-use start permit is returned, bound to the exact resource UID
and runtime boot identity. Authorization precedes source retrieval and preparation.
Under ADR 0002, lost allocations are never replaced, and lost authorization
replies and runtime restarts receive no new permit. Established execution
uncertainty is an infrastructure failure subject to ADR 0002's event precedence.
User code is never started again for the same task.

## Object storage and results

Owned object references contain location, key, byte size, SHA-256 checksum, and
version or ETag when available. Accepted inputs require a stable version ID or
an ETag enforceable through conditional GET. The runtime uses version-addressed
or conditional reads and fails rather than consume changed inputs.

S3-compatible storage must support atomic completed writes, read-after-write
behavior, conditional or versioned creation and reads, streaming or multipart
transfer, and paginated listing and deletion of owned keys. Operators configure
abandoned multipart-upload cleanup. Presigned URLs are generated on demand,
never persisted.

The runtime uploads logs and artifacts, then publishes the versioned manifest
as the payload commit marker. Retries can confirm identical bytes, never replace
them. ADR 0002 also requires a PostgreSQL completion receipt before cancellation
and the execution deadline. The reconciler validates its manifest and commits
the outcome; publication or workload completion alone cannot establish success.

Cross-store consistency depends on immutable objects, idempotent publication,
authoritative database references, and reconciliation of missing or unreferenced
objects.

## Retention and recovery

Terminal writes record payload and metadata deadlines from current retention
policy; payload retention cannot exceed metadata retention. Later policy changes
do not revise those deadlines. At payload expiration, APIs omit references and
previews and stop issuing URLs, even if cleanup is delayed. Previously issued
URLs expire no later than that deadline.

Cleanup records payload expiration before deletion, waits for ADR 0002's stop
conditions, deletes owned objects, then records completion. Account for delayed
uploads; never delete caller-owned inputs.

Metadata expiration ends public lookup and idempotency even if deletion is
delayed. Delete the task and binding in one transaction that also creates an
operator-only tombstone if payload or execution cleanup is incomplete. Retain
remaining references, provider identity, unresolved allocation intent, stop
evidence, reservations, cleanup version, retry state, and bounded failures.
Hold the admission lock during transfer, preserving the active count. Tombstones
remain until cleanup succeeds without extending public retention.

The deployment platform supplies and operates production PostgreSQL. Kind may
include a disposable instance; integration tests use PostgreSQL, never SQLite.
v0.1 uses versioned migrations in a stop, migrate, start sequence.

Process or Pod restart preserves committed state. Production deployments must
define and restore-test recovery point and recovery time objectives. Records lost
beyond the recovery point have no zero-data-loss, idempotency, or at-most-once
execution guarantee.

After restore, audit owned objects and Agent Sandbox resources before accepting
work; resolve unreferenced resources conservatively. Preserve retained terminal
outcomes. If the audit establishes lost execution identity, authorization, or
timely receipt evidence for a nonterminal task, apply ADR 0002's event precedence;
record infrastructure failure when no higher-priority outcome applies. A manifest
alone cannot establish success. Never restart uncertain work.

Readiness requires PostgreSQL, a compatible schema, the singleton lock, safe
object-storage capabilities, and completion of any required post-restore audit.
Liveness is independent of downstream availability and task success.

## Alternatives considered

| Alternative | Reason rejected |
| --- | --- |
| Task custom resource | Provides persistence, watches, and `resourceVersion`, but does not fit atomic idempotency and admission across records behind an imperative task API. Couples recovery and retention to Kubernetes and etcd |
| SQLite on a persistent volume | Supports one replica, but ties recovery to volume and filesystem behavior and requires another persistence implementation for multiple replicas |
| Object storage | Lacks required transactions, uniqueness, and indexed work discovery |
| SandboxClaim, Sandbox, or Pod state | Does not represent durable acceptance, idempotency, cancellation, result retention, or cleanup |
| Memory | Cannot survive process restart |

## Consequences

Acceptance, idempotency, transitions, admission, and history share transactional
invariants. State and results outlive processes; payloads stay outside PostgreSQL
and etcd. Agent Sandbox remains an execution provider, and v0.1 needs neither
distributed task claims nor a separate queue.

PostgreSQL becomes a production and readiness dependency. Operators own database
security, backup, restore, monitoring, and capacity. Rollouts and replica failure
briefly interrupt the API. Cross-store and external failures require reconciliation;
multiple active replicas require a later design.

## Required follow-up designs

[ADR 0002](0002-task-lifecycle.md) defines lifecycle;
[ADR 0003](0003-task-store.md) defines storage records, transactions, and migrations.
Remaining designs cover:

1. Versioned task-specification and result-manifest contracts, including
   canonical request and object-store profiles
2. Reconciliation and cleanup
3. A recovery runbook with concrete recovery objectives
