# Requirements

Last updated: 2026-09-12

## Purpose

KubeTask executes isolated, short-lived tasks on Kubernetes and maintains durable task state
from acceptance through cleanup.

The requirements define v0.1 behavior and constraints.
Design documents and architecture decision records (ADRs) define the architecture and implementation.
Requirement strength follows RFC 2119 and RFC 8174.
MUST is required, SHOULD may be omitted only with a documented reason, and MAY is optional.

## Primary use case

An LLM orchestrator such as OpenWebUI or Archestra.ai submits generated Python and optional input files.
KubeTask returns task state, stdout, stderr, and generated artifacts.
Agents primarily use the Model Context Protocol (MCP) over Streamable HTTP.
Backend services use an HTTP interface with the same operations.

## Goals

KubeTask v0.1 MUST:

- expose a stable asynchronous API for isolated Python execution on Kubernetes
- preserve task state and results from admission through cleanup, and reconcile incomplete work after restart
- enforce limits on admission, resources, execution time, inputs, logs, and outputs
- transfer inputs and results through object storage without Kubernetes `exec` after completion,
  and preserve existing sandbox and runtime responsibilities

## Non-goals for v0.1

The following are outside the v0.1 scope:

- interactive or long-lived execution sessions
- general-purpose Kubernetes batch scheduling
- multi-language execution, including JavaScript
- Docker, Podman, or multiple permanent production execution backends
- implementation of sandbox pooling or sandbox CRD reconciliation
- implementation of container runtime isolation
- package ecosystem support beyond the explicitly selected Python dependency policy

## Actors

| Actor            | Responsibility                                                             |
| ---------------- | -------------------------------------------------------------------------- |
| Client           | Submits, reads, or cancels tasks through MCP or HTTP                       |
| Operator         | Configures, deploys, and observes the service                              |
| Control plane    | Owns admission, task state, orchestration, reconciliation, and cleanup     |
| Task runtime     | Prepares the workspace, executes code, and publishes results               |
| Sandbox provider | Owns isolated execution-environment lifecycle                              |
| Object store     | Stores task specifications, source, inputs, logs, manifests, and artifacts |

## Terms and document boundaries

Requirement IDs provide stable references for design and validation.
The reference profile at the end of the document defines numeric limits and
durations unless an operator configures an allowed override.

| Term | Definition |
| --- | --- |
| Accepted task | A request becomes an accepted task when it is committed to the authoritative task store after validation and admission. Generating an ID or staging a payload alone does not accept a task. |
| Pending task | An accepted task waits for capacity under the active task limit before allocation begins. |
| Active task | A task holds capacity for allocation, workspace preparation, user-code execution, or result publication. A terminal outcome alone does not prove that the execution environment has stopped. |
| Execution authorization | The control plane records irreversible permission to start user code before any external action can start it, as required by ADR 0001. |
| Runtime completion | The runtime completes when it publishes the final result manifest after the uploads referenced in that manifest. User-process exit alone does not complete the runtime. |
| Terminal outcome | The control plane records an immutable execution result. Cleanup and retention progress are tracked separately and cannot revise that result. |
| Payloads | KubeTask owns the task specification, source, staged inputs, logs, result manifests, and artifacts. Cleanup does not delete caller-owned input objects. |

The terms define behavior without specifying the complete set of task states.
[ADR 0001](ADR/0001-authoritative-task-state.md) records the accepted persistence design.
The follow-up designs listed below define exact transitions, protocol schemas, and recovery mechanisms.

## Functional requirements

### Task submission and identity

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-001 | MUST | A client can submit a task containing inline UTF-8 Python source, an optional execution timeout, optional input references, and optional client metadata. The timeout is the only client-selectable resource limit. CPU, memory, workspace, and process limits are operator-controlled. |
| FR-002 | MUST | Submission returns an internal, immutable task ID only after validation and durable acceptance. Acceptance atomically records the task, any applicable idempotency binding, the admission decision, and the initial transition as specified in ADR 0001. |
| FR-003 | MUST | Client-provided identifiers are treated as metadata and never used directly as filesystem paths or Kubernetes resource names. |
| FR-004 | MUST | An optional idempotency key applies across the deployment and binds an accepted client request to one task until task metadata expires. The same key and request return the existing task. The same key with a different request returns a deterministic conflict. The submission rules below define request equivalence and key reuse. |
| FR-005 | MUST | Task submission is asynchronous and does not require the client connection to remain open for the duration of execution. |
| FR-006 | MUST | The `run_task` operation submits through the same asynchronous admission path and waits up to the configured wait limit. It returns the task ID and current state, including the result if the task is terminal. Reaching the wait limit or losing the client connection does not cancel the task. |
| FR-007 | MUST | Client metadata has defined size limits and is stored with the task. Logs include metadata keys and only values allowed by the operator. Client metadata is not used in metric labels or copied directly to Kubernetes labels. It never establishes identity, ownership, authorization, or quota scope. |

### Submission semantics

FR-004 binds the key to a canonical representation of the client request before operator defaults are applied.
Transport encoding and map ordering do not affect whether requests are equivalent.
Source contents, metadata values, and the presence of optional fields affect equivalence.
The contract design defines and versions the canonical representation.

First, the service parses the request and checks its size.
Second, it checks for a retained idempotency binding before applying current admission, policy,
or external input checks. A matching request returns the existing task even if the queue is now full,
defaults have changed, or input objects have been removed. Repeating a request does not extend retention.

The service processes a request as a new submission if its binding is absent or expired.
Rejected submissions do not reserve the key. Without a key, a client that retries after losing
the acceptance response may create another task.

### Validation and admission

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-010 | MUST | The service fully validates each new task request before durable acceptance and before creating an execution resource. A request that matches a retained idempotency binding follows FR-004 and does not repeat admission or input-object checks. |
| FR-011 | MUST | Request validation covers source and specification size, client metadata, requested execution timeout, and input reference count, size, identity, and workspace paths. |
| FR-012 | MUST | Every configurable resource value has an operator-defined minimum, maximum, and default where a default is meaningful. |
| FR-013 | MUST | Omitted values and explicit zeros remain distinct. For a new submission, an omitted execution timeout uses the operator default. The service rejects a supplied timeout outside the configured range, including zero, instead of replacing it with a default. |
| FR-014 | MUST | Admission atomically enforces the pending limit when accepting new tasks and the active limit when dispatching tasks. Concurrent requests and reconciliation cannot accept or dispatch tasks beyond the available capacity under the applicable limit. |
| FR-015 | MUST | A request rejected for validation, policy, unsupported behavior, or saturation creates no task record, idempotency binding, or execution resource. Cleanup removes unreferenced payloads staged before a failed acceptance according to ADR 0001. |
| FR-016 | MUST | Validation and admission failures are distinguishable from dispatch, infrastructure, and user-code failures. |
| FR-017 | MUST | If a task requests unsupported input or execution behavior, the service rejects it before dispatch. |
| FR-018 | SHOULD | Admission limits can be changed through operator configuration without rebuilding the service. |

### Task state and history

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-020 | MUST | The service maintains an explicit task state machine with documented valid transitions and terminal states. |
| FR-021 | MUST | Terminal task outcomes distinguish at least success, user-code failure, cancellation, timeout, expiration, and infrastructure failure. Submission rejection is a pre-task error, not a terminal task outcome. |
| FR-022 | MUST | A task record contains its state and version, state timestamps, requested timeout, effective deadlines, cancellation intent, execution identity and authorization, and result references when available. The record tracks cleanup progress separately. |
| FR-023 | MUST | A client can retrieve task state and retained terminal metadata by internal task ID until metadata retention expires. The API reports payload availability separately, and changes in availability never change the execution outcome. |
| FR-024 | MUST | State changes are atomic and safe under concurrent API and reconciliation activity. |
| FR-025 | MUST | Invalid or stale transition attempts cannot move a task backward or overwrite a terminal outcome. |
| FR-026 | SHOULD | The service retains a compact transition history sufficient for operational diagnosis. |

### Task dispatch and execution

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-030 | MUST | The reconciler processes each accepted task independently of the submission request. |
| FR-031 | MUST | The control plane creates or allocates at most one active execution environment for a task. |
| FR-032 | MUST | The task runtime receives a versioned task specification and a minimal task reference. Source is not embedded in a Pod command. |
| FR-033 | MUST | The initial runtime executes Python source in a bounded, non-interactive process. |
| FR-034 | MUST | The runtime captures stdout and stderr within configured limits and records the user-process exit code when available. Results distinguish a reported exit code from cases where user code did not start or no exit code could be observed. |
| FR-035 | MUST | The runtime enforces the execution deadline and stops the user-code process tree independently of the API connection and control-plane availability. The deadline rules below define the execution budget. |
| FR-036 | MUST | The service maps sandbox allocation, runtime, storage, and user-code failures to stable task-level error categories. |
| FR-037 | MUST | A control-plane restart resumes reconciliation from durable state without creating a second execution attempt for a task whose execution has already been authorized. |
| FR-038 | SHOULD | The runtime protocol permits runtime implementation or version changes without changing the public task API. |
| FR-039 | MUST | User code receives at most one execution attempt per task. Allocation and preparation may be retried only before irreversible execution authorization and within existing phase deadlines. Authorization is persisted before any action can start user code. After authorization, no replacement environment or second user-code start is permitted, including when the first start outcome is unknown. |

### Inputs and workspace

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-040 | MUST | An input reference contains a workspace-relative `path`, an object `key`, and optional `expected_size` and `etag` values. All input references use one operator-configured S3 bucket and prefix. Arbitrary URLs and inline base64 input files are not supported. |
| FR-041 | MUST | Input paths are relative, normalized, and contained within the task workspace. |
| FR-042 | MUST | The service deterministically rejects duplicate or absolute input paths, paths that allow directory traversal, and other unsafe paths. Input paths cannot overwrite runtime-managed source or protocol files or occupy the reserved outputs directory. |
| FR-043 | MUST | The runtime verifies configured input count and byte limits while preparing inputs in the workspace, including when stored metadata is incorrect. |
| FR-044 | MUST | The writable workspace has a bounded storage allocation. |
| FR-045 | MUST | Input retrieval failure produces a task-level failure and does not start user code with an incomplete workspace. |
| FR-046 | MUST | Tasks cannot request dependency installation. The official runtime image includes NumPy, pandas, SciPy, Matplotlib, Seaborn, Pillow, openpyxl, XlsxWriter, python-docx, pypdf, and ReportLab. The configured image determines which preinstalled dependencies are available. Python standard-library and workspace modules are also permitted. |
| FR-047 | MUST | The runtime creates `/workspace/outputs` and sets `KUBETASK_OUTPUT_DIR` to that directory. User code runs with `/workspace` as its working directory. |
| FR-048 | MUST | Before accepting a new task, the control plane verifies each input object and records a stable version or an ETag enforceable by conditional read. The runtime reads the accepted object version or uses a conditional read with the recorded ETag. It fails the task if it cannot read the accepted content and never silently consumes changed content. |
| FR-049 | MUST | The control plane durably stores immutable source and effective task specification before acceptance and gives the runtime task-scoped access to them and the validated input objects. |

### Results, logs, and artifacts

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-050 | MUST | The runtime publishes a versioned result manifest after uploading the logs and artifacts it references. A published manifest is immutable. Retries can confirm identical contents but cannot replace them. Success requires a validated manifest confirming a zero user-code exit code, completion of required transfers, and compliance with output limits. Workload exit alone cannot establish success. For cancellation, timeout, and infrastructure failure, the control plane may finalize without a manifest under the outcome rules below. |
| FR-051 | MUST | The runtime recursively collects regular files from `/workspace/outputs`. It does not follow symlinks or collect files outside that directory. The API does not accept output glob patterns. |
| FR-052 | MUST | The runtime enforces limits on per-file size, total bytes, and file count while collecting outputs. Exceeding a limit reports `output_limit_exceeded` as an execution failure, subject to FR-064. Bounded logs and artifacts already uploaded and validated remain available as partial results. |
| FR-053 | MUST | Artifacts are transferred directly to object storage. They are not carried through Kubernetes API responses or control-plane memory as a complete archive. |
| FR-054 | MUST | The control plane does not depend on executing commands in a terminated container to obtain results. |
| FR-055 | MUST | A client can list artifact metadata from a retained terminal result and request task-scoped presigned GET URLs before payload retention expires. URLs are not persisted as task state. |
| FR-056 | MUST | Metadata for a retained artifact includes its logical path, object reference, size, and media type when known. After payload expiration, APIs omit the object reference and report expiration under FR-074. |
| FR-057 | MUST | Missing required manifests and invalid, oversized, or conflicting manifests produce stable infrastructure diagnostics and cannot establish success. Manifest diagnostics do not overwrite an established cancellation, timeout, or other terminal outcome. |
| FR-058 | MUST | A log or artifact presigned GET URL is valid for at most the configured URL lifetime and never beyond the payload retention deadline. URLs can be regenerated only before that deadline. |
| FR-059 | MUST | When logs are available and retained, task results include bounded stdout and stderr previews, truncation metadata, byte counts, and references to captured streams. Results explicitly report missing or expired logs and never represent them as zero-byte output. |

### Cancellation and deadlines

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-060 | MUST | A client can request cancellation of a non-terminal task. |
| FR-061 | MUST | Cancellation is idempotent and records durable intent before execution resources are changed. Repeating cancellation for a retained terminal task returns its existing outcome without changing it. A cancellation request does not guarantee that execution resources have already stopped. |
| FR-062 | MUST | The reconciler stops user-code execution and releases execution resources for canceled tasks. Stop and cleanup failures remain visible and are retried under NFR-023 and NFR-024. Cancellation does not depend on the client connection remaining open. |
| FR-063 | MUST | A task reaches the distinct `TimedOut` outcome if its execution budget expires before a qualifying completion, subject to the race rules in FR-064. The control plane can finalize the task without a runtime timeout manifest. |
| FR-064 | MUST | The lifecycle ADR defines which event takes precedence when completion evidence, failure, durable cancellation intent, and phase deadlines conflict. It covers simultaneous events and delayed observations. The rules obey the outcome and deadline constraints below and never revise a committed terminal outcome. |
| FR-065 | SHOULD | Partial logs and artifacts that were safely published and validated before cancellation or timeout remain discoverable while retained and are identified as partial. Unvalidated or unpublished files are not promised as results. |
| FR-066 | MUST | A task reaches the `Expired` terminal outcome if it reaches its pending deadline before dispatch, subject to FR-064. The `Expired` outcome applies to queue expiration. Payload or metadata retention expiration does not change the execution outcome. |
| FR-067 | MUST | A task that reaches its allocation deadline before the runtime is ready for preparation ends as an infrastructure failure, subject to FR-064. Allocation retries do not reset this deadline. |

### Outcome and deadline constraints

The outcome and deadline rules apply to FR-050 and FR-060 through FR-067.

First, pending time starts at durable acceptance and ends when active capacity is reserved for dispatch.
Second, allocation time starts at that reservation and ends at the handoff to runtime preparation.
Third, the execution budget starts at the same handoff, before preparation for the task begins.
No interval between phases is left without a deadline.

The execution budget covers specification and source retrieval, workspace preparation,
input retrieval, user-code execution, output collection, and final manifest publication.
Allocation is excluded. Retries never extend or restart the applicable deadline.

All runtime execution and result uploads must fit within the execution budget.
There is no extra upload allowance after its deadline.
If required publication cannot finish in time, the task times out subject to FR-064.
Control-plane observation, validation, and cleanup can finish after the deadline.

Success requires validated evidence that the runtime completed before the execution deadline.
A late observation of a timely completion does not by itself cause a timeout.
User-code exit before the deadline is insufficient if result publication did not also complete in time.

The runtime must attempt to publish bounded partial results during a controlled stop when time
and storage access remain. Timeout, forced termination, or storage failure can prevent publication.
Cancellation or expiration while a task is pending does not require a runtime or manifest.

The control plane can establish cancellation, timeout, queue expiration, or infrastructure failure
from durable intent and validated observations without a manifest.
A missing manifest alone does not prove cancellation or timeout.
An unexplained runtime loss without a valid manifest is an infrastructure failure.

Manifest errors remain visible as diagnostics but cannot replace an outcome already established
by the lifecycle precedence rules. Result publication and diagnostics arriving after a terminal
commit cannot revise the recorded outcome.

Persisted timestamps and deadlines use PostgreSQL time as required by ADR 0001.
The lifecycle and runtime designs must define how completion timing is validated without ordering
events by unsynchronized client clocks.

The lifecycle ADR must resolve the remaining transitions and event precedence before implementation.
It must also define when active capacity is released. An environment that may still execute
code cannot permit unsafe replacement or admission beyond the active limit.

### Reconciliation and cleanup

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-070 | MUST | The service continuously reconciles desired KubeTask task state with durable task data, sandbox or workload state, and result manifests. |
| FR-071 | MUST | Reconciliation is idempotent and safe to retry after partial failure. |
| FR-072 | MUST | The reconciler detects missing execution resources, stale non-terminal tasks, completed runtimes awaiting finalization, and orphaned resources owned by KubeTask. |
| FR-073 | MUST | Cleanup state is tracked separately from the task's execution outcome so cleanup failure cannot erase the result. |
| FR-074 | MUST | Payload and task-metadata retention are separate. At payload expiration, APIs stop returning payload references, log previews, and new download URLs even if deletion is incomplete. At metadata expiration, public task lookup and the task's idempotency binding expire together. Cleanup progress and tombstones follow the retention rules below. |
| FR-075 | MUST | Cleanup retries transient failures and exposes persistent cleanup failures to operators. |
| FR-076 | MUST | KubeTask does not reconcile internal lifecycle state owned by the sandbox provider. |

### Retention semantics

When a task becomes terminal, the control plane records separate payload and metadata expiration
timestamps using the configured retention periods. Later changes to retention defaults do not
change the recorded timestamps. Payload retention cannot exceed metadata retention.

Payload expiration applies to previews and all KubeTask-owned payload references, including partial results.
First, the control plane records logical expiration, which ends public access.
Second, it deletes the payloads.
Third, it records cleanup completion. Reads enforce the recorded deadlines even if a cleanup worker is delayed.
Previously issued URLs expire no later than the same deadline.
Objects that remain in storage after expiration do not remain publicly available.

Metadata expiration ends public task lookup and idempotency together.
Expired metadata is unavailable even if database deletion is pending.
Reusing an expired key can create a new task with a new internal ID.

If cleanup remains incomplete, a tombstone accessible only to operators retains references
to the remaining KubeTask-owned resources and the retry state. The tombstone does not retain
the public task or key binding. Cleanup never deletes caller-owned inputs.
Physical deletion can be retried and may finish after logical expiration.

### Interfaces

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-080 | MUST | The service provides versioned `submit_task`, `run_task`, `get_task`, and `cancel_task` operations and an operation to list artifacts. |
| FR-081 | MUST | HTTP and MCP interfaces translate to the same application behavior and task model. |
| FR-082 | MUST | Public errors use stable machine-readable codes and do not expose internal credentials, stack traces, or raw infrastructure objects. |
| FR-083 | MUST | Unknown fields and unsupported protocol versions have documented, deterministic handling. |
| FR-084 | MUST | The HTTP interface supports request correlation and standard readiness and liveness endpoints. |
| FR-085 | MUST | Deployed MCP endpoints use Streamable HTTP. |
| FR-086 | MAY | A stdio MCP transport may be provided for local development. |

### Operator configuration

| ID | Strength | Requirement |
| --- | --- | --- |
| FR-090 | MUST | Operators can configure resource and admission limits, runtime image, sandbox template or class, storage location, retention, and reconciliation timing. Effective resource limits and time limits for each phase are persisted at task acceptance. Later default changes do not change those recorded limits. |
| FR-091 | MUST | Configuration is validated before the service reports readiness. |
| FR-092 | MUST | Invalid security-critical configuration fails closed. |
| FR-093 | SHOULD | Configuration reports effective non-secret values for diagnostics. |

Lowering a global admission limit below current usage blocks new admissions or dispatches until
capacity is available. It does not cancel accepted tasks or change their persisted per-task limits.

## Non-functional requirements

### Security

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-001 | MUST | The threat model assumes submitted code and input files are malicious. |
| NFR-002 | MUST | Task workloads comply with the Kubernetes Restricted Pod Security Standard and run as non-root. They disallow privilege escalation, drop Linux capabilities, and do not use `hostPath`. They do not run as privileged containers. |
| NFR-003 | MUST | Task workloads receive no Kubernetes service-account token unless a documented runtime integration requires one. |
| NFR-004 | MUST | Each task receives only short-lived, task-scoped access to the object keys it must read or write. Long-lived infrastructure credentials are not exposed to user code. |
| NFR-005 | MUST | Task egress is denied by default. Only DNS and operator-configured infrastructure endpoints required by the runtime are permitted. User code has no general internet or package-repository access. |
| NFR-006 | MUST | The service enforces limits on CPU, memory, ephemeral storage, process count, process duration, inputs, outputs, and logs. |
| NFR-007 | MUST | Paths from requests, archives, manifests, and runtime output are normalized and checked for containment before filesystem use. |
| NFR-008 | MUST | Service and infrastructure credentials are redacted from API errors, logs, metrics, workload metadata, and persisted task status. |
| NFR-009 | MUST | Kubernetes RBAC and object-store permissions follow least privilege. |
| NFR-010 | MUST | Static source scanning, if provided, is treated only as defense in depth and not as the isolation boundary. |
| NFR-011 | SHOULD | The runtime supports a configurable Kubernetes `RuntimeClass` for stronger isolation such as gVisor or Kata Containers. |
| NFR-012 | MUST | KubeTask v0.1 serves a single tenant. Platform network controls restrict HTTP and MCP access to trusted internal callers. Application authentication and per-user task authorization are outside the v0.1 deployment scope. Execution-resource ownership checks remain required. Public or multi-user deployment requires a separate authentication and authorization design. The service must determine tenant and principal identifiers without treating client metadata as identity. |
| NFR-013 | MUST | Source, inputs, stdout, stderr, and artifact contents are not written to operational logs by default. |
| NFR-014 | MUST | Each task workload has an enforced process-count limit. |

### Reliability and consistency

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-020 | MUST | Accepted task state survives process restart and loss of a control-plane replica. |
| NFR-021 | MUST | At-least-once reconciliation does not create more than one active execution environment per task. |
| NFR-022 | MUST | State updates use concurrency control to prevent stale writers from silently overwriting newer state. |
| NFR-023 | MUST | Safe, idempotent operations are retried after transient dependency failures with bounded exponential backoff and jitter. Retry state survives restart. Retries obey phase deadlines and FR-039 and never repeat a user-code start whose outcome is uncertain. |
| NFR-024 | MUST | Permanent failures and exhausted retries are visible through task state or operator signals. The service does not retry them indefinitely without reporting them. |
| NFR-025 | MUST | The service produces deterministic outcomes for duplicate submissions and repeated cancellation requests. |
| NFR-026 | SHOULD | The control plane supports multiple replicas without task duplication or split-brain state transitions. ADR 0001 defers this requirement beyond v0.1 and selects one replica with one active reconciler. |
| NFR-027 | MUST | Operators document PostgreSQL backup and restore procedures for production deployments, including recovery point and recovery time objectives. Restore tests verify those procedures and objectives. Recovery includes the external-resource audit required by ADR 0001 and never automatically reruns work whose execution authorization may have been lost. |

Process restart and loss of a control-plane replica preserve committed state.
Recovery from a backup is limited by the deployment's recovery point objective,
which defines how much recent data may be lost.
Task and idempotency guarantees do not cover records lost beyond that recovery point.
The audit in NFR-027 prevents automatic execution of restored tasks whose previous execution is uncertain.

### Performance and capacity

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-030 | MUST | Submission, task reads, and cancellation do not wait for user-code completion. FR-006 defines the bounded wait provided by `run_task`. |
| NFR-031 | MUST | The service applies backpressure and rejects excess work rather than creating an unbounded in-memory or Kubernetes queue. |
| NFR-032 | MUST | Artifact transfers use streaming or chunks so control-plane memory usage does not grow linearly with artifact size. |
| NFR-033 | MUST | Control-plane CPU and memory consumption are bounded by operator-configurable concurrency and queue limits. |
| NFR-034 | SHOULD | The implementation has documented capacity-test results for the supported task concurrency before v0.1 release. |
| NFR-035 | MUST | Before production readiness, reproducible tests verify the reference resource limits, API latency objectives, and restart recovery objective. Reports identify the workload, arrival rate, concurrency, payload sizes, sample counts, dependency versions, and infrastructure. Monthly availability is measured in deployed operation as defined below. |

### Availability and recovery

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-040 | MUST | The readiness endpoint reports not ready when the service cannot safely accept tasks because of invalid configuration or loss of an authoritative dependency. |
| NFR-041 | MUST | Liveness does not depend on the success of an individual task or transient availability of a downstream dependency. |
| NFR-042 | MUST | After restart, reconciliation resumes incomplete task processing without operator intervention under normal dependency recovery. |
| NFR-043 | SHOULD | Planned control-plane rollout does not terminate running task workloads solely because an API replica stops. |

### Observability and operability

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-050 | MUST | Logs are structured and include internal task ID, correlation ID, component, and state transition where applicable. |
| NFR-051 | MUST | Metrics include submitted requests, accepted tasks, and rejected submissions, along with counts of pending, active, and terminal tasks. They also include queue wait, allocation and execution duration, artifact bytes, reconciliation errors, cleanup failures, and downstream retries. |
| NFR-052 | MUST | Metrics do not contain unbounded task IDs, external IDs, filenames, or other high-cardinality labels. |
| NFR-053 | MUST | Every state transition and rejection can be correlated with an operational log record. |
| NFR-054 | MUST | Prometheus metrics are sufficient for v0.1. Distributed tracing is not required. |
| NFR-055 | SHOULD | Operator documentation covers installation, configuration, dependency failure, stuck-task diagnosis, cancellation, cleanup failure, and recovery. |

### Maintainability and compatibility

| ID | Strength | Requirement |
| --- | --- | --- |
| NFR-060 | MUST | Control-plane domain types do not use sandbox-provider types, Kubernetes Pod types, or HTTP and MCP data transfer objects as the task domain model. |
| NFR-061 | MUST | Task specification and result manifest formats are independently versioned and validated. |
| NFR-062 | MUST | Runtime and sandbox-provider capabilities are explicit. The service rejects unsupported behavior instead of silently reducing functionality. |
| NFR-063 | MUST | Behavior, failure cases, state transitions, idempotency, and reconciliation have automated tests. |
| NFR-064 | MUST | Changes pass formatting, static analysis, unit tests, and relevant integration tests before merge. |
| NFR-065 | SHOULD | Integration tests cover a real Kubernetes environment, sandbox provider, and S3-compatible object store. |
| NFR-066 | MUST | Meaningful architectural trade-offs are recorded as ADRs before implementation. |
| NFR-067 | MUST | Official runtime images use dependencies pinned to specific versions. Each release publishes a package inventory or software bill of materials. |

## Project constraints

The project has the following design constraints:

- the control plane is implemented in Go
- the initial task runtime is implemented in Python
- the service runs on Kubernetes
- Agent Sandbox is the only production sandbox lifecycle provider in v0.1
- PostgreSQL is authoritative for task state and coordination under ADR 0001
- S3-compatible object storage holds payloads under the capability profile required by ADR 0001
- v0.1 uses one control-plane replica and one active reconciler under ADR 0001
- the deployment platform provides production PostgreSQL, and local integration
  tests may use a disposable PostgreSQL instance
- `llm-sandbox` may be used inside a runtime only where it provides clear value
- KubeTask must not reproduce lifecycle or runtime functionality already owned
  by Agent Sandbox or `llm-sandbox` without a documented reason

Changing a project constraint requires an ADR.
Domain code remains independent PostgreSQL driver types and object-store SDK types
as well as the transport and sandbox-provider types covered by NFR-060.

## Initial quantitative limits and service objectives

The reference profile defines the initial values for acceptance testing.
Operators may configure stricter resource and admission limits.
Higher resource or admission limits require capacity validation.
Retention and URL lifetimes are operator-configurable subject to the retention rules above;
the URL lifetime cannot exceed the reference maximum.
Default changes apply to future tasks or future terminal retention calculations as specified above.

Latency and availability are service objectives.
Validation uses the documented workload and environment required by NFR-035.
The objectives are targets and do not represent measured capacity or published test results.

| Target                                     | Initial v0.1 value                                                  |
| ------------------------------------------ | ------------------------------------------------------------------- |
| Maximum source and task specification size | 1 MiB combined                                                      |
| Maximum client metadata                    | 16 entries; 64 bytes per key; 256 bytes per value; 4 KiB total      |
| Maximum input files                        | 20 files                                                            |
| Maximum input size                         | 50 MiB per file; 100 MiB aggregate                                  |
| Maximum output files                       | 100 files                                                           |
| Maximum artifact size                      | 50 MiB per file; 250 MiB aggregate                                  |
| Maximum stdout                             | 1 MiB                                                               |
| Maximum stderr                             | 1 MiB                                                               |
| Maximum inline stdout and stderr preview   | 64 KiB per stream                                                   |
| Maximum processes per task                 | 256                                                                 |
| CPU                                        | 100m minimum; 1 CPU default; 2 CPU maximum                          |
| Memory                                     | 256 MiB minimum; 512 MiB default; 4 GiB maximum                     |
| Maximum ephemeral workspace                | 1 GiB                                                               |
| Task execution time                        | 1 second minimum; 60 seconds default; 300 seconds maximum           |
| Maximum pending time                       | 5 minutes                                                           |
| Maximum sandbox allocation time            | 2 minutes                                                           |
| `run_task` wait time                       | 60 seconds                                                          |
| Default active task limit                  | 20 tasks per deployment                                             |
| Default pending task limit                 | 100 tasks per deployment                                            |
| Submit API latency                         | p95 at most 500 ms; p99 at most 1 second                            |
| Get and cancel API latency                 | p95 at most 250 ms; p99 at most 500 ms                              |
| Reconciliation recovery after restart      | All incomplete tasks reconsidered within 60 seconds after readiness |
| Task metadata retention                    | 30 days after terminal state                                        |
| Task payload retention                     | 7 days after terminal state                                         |
| Maximum presigned GET URL lifetime         | 15 minutes                                                          |
| Control-plane availability                 | 99.5% per calendar month                                            |

`MiB` and `GiB` are binary units. Input and output limits count uncompressed bytes.

Results explicitly report stream truncation.
A truncated preview preserves the beginning and end of the captured stream.
Results report captured and observed byte counts, marking the observed count as incomplete if draining ended abruptly.
Unknown counts and unavailable streams are never represented as zero.
The runtime continues draining stdout and stderr after reaching capture limits so user code cannot block on a full pipe.

Execution deadlines follow the outcome and deadline constraints above.
Operator defaults and bounds apply to the entire execution budget.
Active and pending limits define admission defaults and do not represent tested platform capacity.
Retention starts when a task becomes terminal.

API latency is measured from receipt of the complete request to the response at the service boundary.
Submission includes validation, input-object checks, payload staging, and durable acceptance,
but excludes dispatch and execution.
Cancellation latency measures the time to record durable intent. It does not measure the time to stop execution.
The deliberate wait performed by `run_task` is excluded from submission API latency.

Availability is measured per calendar month as the fraction of well-formed, supported API requests
served according to their contract.
Service or required dependency failures and saturation rejections count as unavailable.
Expected validation or policy responses, idempotency conflicts, and missing-task responses do not count as unavailable.
A returned user-code failure is a successful API operation.

## Required follow-up designs

ADR 0001 is accepted. Implementation of the corresponding behavior requires
reviewed and accepted designs for the following areas:

- Task lifecycle: complete transition table, phase boundaries, race precedence,
  execution authorization, completion evidence, active-capacity release, and
  recovery after uncertain external operations
- Versioned task specification and result manifest: field types, bounds,
  canonicalization, reserved paths, integrity validation, and partial results
- Runtime and Agent Sandbox contract: allocation, preparation, start gating,
  cancellation, deadline enforcement, scoped credentials, and capability checks
- HTTP and MCP contracts: resources and tool schemas, stable errors, unknown
  fields, version handling, and expired or unavailable results
- PostgreSQL schema and migrations, reconciliation scheduling, cleanup,
  tombstones, and recovery procedures with restore validation
- Official runtime image: package versions, build and update policy, and package
  inventory or software bill of materials

The designs must preserve the requirements and the accepted ADR. Any proposed
change to observable behavior or v0.1 scope must be identified for review before
implementation.
