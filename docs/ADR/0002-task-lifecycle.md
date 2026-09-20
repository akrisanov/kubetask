# ADR 0002: Task lifecycle

- Status: Accepted
- Date: 2026-09-20
- Depends on: [ADR 0001](0001-authoritative-task-state.md)

## Context

Tasks must survive control-plane restarts without repeating user code or losing results.
Request-scoped execution and cleanup cannot provide that guarantee.

Keep ADR 0001's PostgreSQL authority, S3 payload storage, and single reconciler.
Agent Sandbox remains the only production provider. Local execution is for trusted development code,
not hostile-code containment. Hardened isolation is outside v0.1 unless deliberately added.

## Decision

### States and transitions

`Pending` means accepted and waiting for capacity.
`Allocating` holds capacity while creating a runtime that cannot yet execute code.
`Running` means execution is authorized and includes preparation, execution, and result publication.
It does not prove that user code started.

| From         | Allowed next states                            | Condition                                                                                          |
| ------------ | ---------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| No task      | `Pending`                                      | Durable acceptance under ADR 0001                                                                  |
| `Pending`    | `Allocating`                                   | Capacity available, no cancellation, before pending deadline                                       |
| `Pending`    | `Cancelled`, `Expired`, `Failed`               | Cancellation, pending deadline, or permanent infrastructure failure                                |
| `Allocating` | `Running`                                      | Exact runtime identity recorded, no cancellation, authorization granted before allocation deadline |
| `Allocating` | `Cancelled`, `Failed`                          | Cancellation, allocation deadline, or infrastructure failure                                       |
| `Running`    | `Succeeded`, `Failed`, `Cancelled`, `TimedOut` | Completion, failure, cancellation, or execution deadline                                           |
| Terminal     | None                                           | Outcome is immutable                                                                               |

`Failed` distinguishes execution errors from infrastructure errors.
Rejection creates no task. Cancellation intent, completion evidence, capacity reservation,
and cleanup are separate facts, not additional states.

Transitions check state and version and append history atomically.
Capacity changes lock the admission row before the task row.
External calls stay outside transactions.
Sample PostgreSQL time after acquiring locks; task versions order events with equal timestamps.

### Execution and deadlines

Persist allocation intent and a deterministic name before creating a runtime.
Resolve uncertain creates by that identity and verify ownership.
Never adopt a different UID or replace a lost allocation for the same task.

The runtime requests authorization before fetching source or preparing inputs.
One transaction changes `Allocating` to `Running`, binds the resource UID and
runtime boot identity, and sets the execution deadline. Only that request gets
a start permit, after commit. The permit is consumed once and never replayed.
A lost reply can consume the attempt without starting code. Runtime and Pod
restarts cannot receive another permit. Established start uncertainty produces
an infrastructure failure, not another attempt.

Pending time starts at acceptance; allocation time starts when capacity is
reserved; execution time starts at authorization. The execution budget includes
preparation, uploads, and the completion receipt. Retries never reset deadlines.

The runtime enforces its budget independently of control-plane availability,
using elapsed time from before its authorization request, including request
latency and suspension. It drains bounded logs and stops the process tree,
including children left after parent exit. A conservative early deadline stop
waits for the database deadline to establish `TimedOut`. Client disconnects do
not cancel tasks.

### Completion and cancellation

After uploading logs and artifacts, the runtime publishes an immutable manifest
and submits its reference and digest to PostgreSQL. This **completion receipt**
is accepted only for the authorized runtime, before cancellation and before the
execution deadline. Identical retries return the original receipt; conflicting
contents cannot replace it. Cancel and timeout reports provide partial results,
not eligible completion receipts.

The reconciler validates the manifest and referenced objects before recording
an outcome. A zero exit alone is insufficient. Validation retries have a
60-second window from receipt acceptance, followed by one final bounded read.
Restart does not reset that window. Invalid or unverifiable evidence produces
infrastructure failure. A lost S3 write response requires checking the stored
digest; a conditional-write conflict alone does not prove identical contents.

Cancellation durably records its first intent and returns the current task.
Repeated requests do not change that intent or an existing terminal outcome.
Apply events in this order:

1. Preserve a terminal outcome
2. Resolve an eligible completion receipt, including validation failure
3. Honor cancellation recorded before the current phase deadline
4. Apply the phase deadline: `Expired`, allocation failure, or `TimedOut`
5. Apply an established failure; otherwise continue

At the deadline, the deadline wins. A receipt preceding cancellation keeps its
priority while validation runs. Cancellation requests stop execution but do not
prove it has stopped or undo effects already produced. Late validated partial
results may be exposed while retained, without changing outcome or retention.

### Capacity and recovery

Reserve capacity at dispatch. Release it only after allocation requests are
resolved, all allocated runtimes and their children are stopped, and the
provider cannot recreate them. A terminal outcome, missing Pod, or successful
delete request is insufficient. Keep uncertain reservations visible for operator
resolution rather than expiring them automatically.

At metadata expiration, atomically move any unreleased reservation to the
cleanup tombstone. Admission counts reservations in both task rows and
tombstones. Object cleanup can proceed separately after execution stops and
must account for delayed uploads before declaring completion.

Restart resumes observation and cleanup from durable state, never a start
permit. Lock loss stops new mutations and dispatch; it does not cancel external
requests already in flight. Control-plane shutdown must not invoke SDK cleanup
that deletes running task resources. Local recovery must verify process identity,
not rely on a PID alone. Database restore follows ADR 0001's audit and never
reruns uncertain work or infers timely completion from a manifest alone.

## Tradeoffs and amendments

At most one attempt can mean no execution after a lost authorization reply.
Uncertain termination can block capacity until an operator resolves it.
These costs preserve the execution and admission guarantees without a retry engine or
node-fencing service.

This ADR amends the requirements and ADR 0001 in two areas:

- **Completion receipt:** without a timely receipt, even a successfully uploaded
  result can time out during a control-plane outage. This replaces the previous
  publication-only completion rule and narrows success recovery after restore
- **Retention accounting:** capacity held in cleanup tombstones remains counted
  after task metadata expires, replacing task-row-only admission counts

Separate preparation and finalization states add no behavior needed here.
Runtime message schemas, authentication, and SQL layout belong in their
respective designs, not this ADR.

## Essential acceptance cases

- Lost acceptance reply and duplicate submission return the same retained task
- Concurrent admission and dispatch cannot exceed their respective limits
- Lost create or start replies, restarted runtimes, and reused resource names
  cannot cause a second attempt or premature capacity release
- Cancellation, completion, and deadlines follow the ordering above, including
  equal timestamps and database lock waits
- Successful code with missing or conflicting outputs cannot become success;
  a timely receipt survives restart and late validation
- Delayed creates, unreachable nodes, surviving child processes, and metadata
  expiration preserve capacity until execution is confirmed stopped
