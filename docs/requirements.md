# Requirements

Last updated: 2026-08-08

## Purpose

KubeTask is a Kubernetes-based service for executing isolated, short-lived
tasks. It presents execution as a durable task lifecycle rather than as a
container, Pod, or interactive sandbox session.

This document defines the functional and non-functional requirements for
v0.1. It describes required behavior and constraints. Architectural decisions
and implementation details belong in design documents and ADRs.

The terms **MUST**, **SHOULD**, and **MAY** indicate normative requirement
strength as defined by RFC 2119 and RFC 8174:

- **MUST**: an absolute requirement for v0.1
- **SHOULD**: recommended for v0.1; omitting it requires a documented reason
- **MAY**: optional for v0.1

## Primary use case

An LLM orchestrator submits generated Python code, execution limits, and
optional input files. KubeTask executes the code in an isolated, short-lived
environment and returns its status, stdout, stderr, and generated artifacts to
the orchestrator.

## Goals

KubeTask v0.1 MUST:

- accept a Python execution task through a stable application-facing API
- execute the task in an isolated Kubernetes workload
- manage the task asynchronously from submission through cleanup
- apply admission, resource, time, and output limits
- make task state and results durable across control-plane restarts
- move task inputs and outputs without depending on Kubernetes `exec` after
  workload completion
- expose task results and artifact metadata in a structured form
- recover incomplete work through task-level reconciliation
- integrate with existing sandbox and runtime projects without duplicating their
  responsibilities

## Non-goals for v0.1

The following are outside the v0.1 scope:

- interactive or long-lived execution sessions
- general-purpose Kubernetes batch scheduling
- a generic multi-language runtime framework
- JavaScript or other non-Python runtimes
- Docker and Podman production backends
- multiple permanent Kubernetes execution backends
- implementation of sandbox pooling or sandbox CRD reconciliation
- implementation of container runtime isolation
- package ecosystem support beyond the explicitly selected Python dependency
  policy

## Actors

| Actor            | Responsibility                                                                                                     |
| ---------------- | ------------------------------------------------------------------------------------------------------------------ |
| Client           | OpenWebUI, Archestra.ai, or another LLM backend or agent that submits, reads, or cancels tasks through MCP or HTTP |
| Operator         | Configures limits, deploys the service, and observes its health                                                    |
| Control plane    | Owns task admission, durable state, orchestration, reconciliation, and cleanup                                     |
| Task runtime     | Prepares a workspace, executes submitted code, and publishes the result before exiting                             |
| Sandbox provider | Owns the lifecycle of the isolated Kubernetes execution environment                                                |
| Object store     | Stores task specifications, source, inputs, logs, result manifests, and artifacts                                  |

## Functional requirements

### Task submission and identity

| ID     | Strength | Requirement                                                                                                                                                                                                               |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| FR-001 | MUST     | A client can submit a task containing inline UTF-8 Python source, an optional execution timeout, optional input references, and optional client metadata.                                                                 |
| FR-002 | MUST     | Submission returns an internal, immutable task identifier after the request has been validated and durably accepted.                                                                                                      |
| FR-003 | MUST     | Client-provided identifiers are treated as metadata and never used directly as filesystem paths or Kubernetes resource names.                                                                                             |
| FR-004 | MUST     | The service supports an idempotency key for task submission. Repeating the same accepted request with the same key returns the same task. Reusing the key for a different request returns a deterministic conflict error. |
| FR-005 | MUST     | Task submission is asynchronous and does not require the client connection to remain open for the duration of execution.                                                                                                  |
| FR-006 | MUST     | The `run_task` convenience operation uses the asynchronous task model and waits up to 60 seconds. If the task is still active, it returns the task ID and current state.                                                  |
| FR-007 | MUST     | A task can include bounded client metadata for correlation. Client metadata is non-authoritative and does not establish identity, ownership, or quota scope.                                                              |

### Validation and admission

| ID     | Strength | Requirement                                                                                                                                                               |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| FR-010 | MUST     | The service validates the complete request before creating a sandbox or workload.                                                                                         |
| FR-011 | MUST     | Request validation covers source and specification size, client metadata, requested execution timeout, and input reference count, size, identity, and workspace paths.    |
| FR-012 | MUST     | Every configurable resource value has an operator-defined minimum, maximum, and default where a default is meaningful.                                                    |
| FR-013 | MUST     | Missing values and explicit zero values have distinct, documented semantics.                                                                                              |
| FR-014 | MUST     | Admission enforces a bounded number of active tasks and a bounded pending queue.                                                                                          |
| FR-015 | MUST     | Requests rejected by validation, policy, or saturation do not create execution resources.                                                                                 |
| FR-016 | MUST     | Validation and admission failures are distinguishable from dispatch, infrastructure, and user-code failures.                                                              |
| FR-017 | MUST     | If a task requests unsupported input or execution behavior, the service rejects it before dispatch.                                                                       |
| FR-018 | SHOULD   | Admission limits can be changed through operator configuration without rebuilding the service.                                                                            |

### Task state and history

| ID     | Strength | Requirement                                                                                                                                                              |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| FR-020 | MUST     | The service maintains an explicit task state machine with documented valid transitions and terminal states.                                                              |
| FR-021 | MUST     | Terminal outcomes distinguish at least success, user-code failure, rejection, cancellation, timeout, expiration, and infrastructure failure.                             |
| FR-022 | MUST     | A task record contains its current state, state timestamps, requested deadline, cancellation intent, associated execution resource, result location, and cleanup status. |
| FR-023 | MUST     | A client can retrieve the current state and terminal result of a task by its internal identifier.                                                                        |
| FR-024 | MUST     | State changes are atomic and safe under concurrent API and reconciliation activity.                                                                                      |
| FR-025 | MUST     | Invalid or stale transition attempts cannot move a task backward or overwrite a terminal outcome.                                                                        |
| FR-026 | SHOULD   | The service retains a compact transition history sufficient for operational diagnosis.                                                                                   |

### Task dispatch and execution

| ID     | Strength | Requirement                                                                                                                                           |
| ------ | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| FR-030 | MUST     | The reconciler processes each accepted task independently of the submission request.                                                                  |
| FR-031 | MUST     | The control plane creates or allocates at most one active execution environment for a task.                                                           |
| FR-032 | MUST     | The task runtime receives a versioned task specification and a minimal task reference rather than source embedded in a Pod command.                   |
| FR-033 | MUST     | The initial runtime executes Python source in a bounded, non-interactive process.                                                                     |
| FR-034 | MUST     | The runtime captures the process exit code and bounded stdout and stderr.                                                                             |
| FR-035 | MUST     | The runtime enforces the task deadline independently of the lifetime of an API request.                                                               |
| FR-036 | MUST     | The service maps sandbox allocation, runtime, storage, and user-code failures to stable task-level error categories.                                  |
| FR-037 | MUST     | A control-plane restart does not resubmit a running task after in-memory state is lost.                                                               |
| FR-038 | SHOULD   | The runtime protocol permits runtime implementation or version changes without changing the public task API.                                          |
| FR-039 | MUST     | Allocation and preparation may be retried before execution starts. After the task enters `Running`, user code receives at most one execution attempt. |

### Inputs and workspace

| ID     | Strength | Requirement                                                                                                                                                                                                                                                                    |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| FR-040 | MUST     | An input reference contains a workspace-relative `path`, an object `key`, and optional `expected_size` and `etag` values for one operator-configured S3 bucket and prefix.                                                                                                     |
| FR-041 | MUST     | Input paths are relative, normalized, and contained within the task workspace.                                                                                                                                                                                                 |
| FR-042 | MUST     | Duplicate, absolute, traversing, or otherwise unsafe input paths are rejected deterministically.                                                                                                                                                                               |
| FR-043 | MUST     | The runtime verifies configured input count and byte limits while materializing inputs, including when stored metadata is incorrect.                                                                                                                                           |
| FR-044 | MUST     | The writable workspace has a bounded storage allocation.                                                                                                                                                                                                                       |
| FR-045 | MUST     | Input retrieval failure produces a task-level failure and does not start user code with an incomplete workspace.                                                                                                                                                               |
| FR-046 | MUST     | Dynamic dependency installation is not supported. The official runtime image includes NumPy, pandas, SciPy, Matplotlib, Seaborn, Pillow, openpyxl, XlsxWriter, python-docx, pypdf, and ReportLab. User code can import only packages included in the configured runtime image. |
| FR-047 | MUST     | The runtime creates `/workspace/outputs`, runs user code with `/workspace` as its working directory, and sets `KUBETASK_OUTPUT_DIR` to the output directory.                                                                                                                   |
| FR-048 | MUST     | Before accepting a task, the control plane verifies that each referenced input object exists and matches the configured size and identity constraints.                                                                                                                         |
| FR-049 | MUST     | The control plane persists submitted source and gives the runtime task-scoped access to the source and validated input objects.                                                                                                                                                |

### Results, logs, and artifacts

| ID     | Strength | Requirement                                                                                                                                                                                                                                |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| FR-050 | MUST     | For every controlled outcome, including success, user-code failure, timeout, and cancellation, the runtime uploads a versioned result manifest before exit. Abrupt termination without a valid manifest becomes an infrastructure failure. |
| FR-051 | MUST     | The runtime recursively collects regular files from `/workspace/outputs`. It does not follow symlinks or collect files outside that directory.                                                                                             |
| FR-052 | MUST     | The runtime enforces per-file size, aggregate byte, and file-count limits while collecting outputs. Exceeding a limit fails the task with `output_limit_exceeded` and preserves bounded logs and uploaded artifacts as partial results.    |
| FR-053 | MUST     | Artifact data is transferred directly to object storage; it is not carried through Kubernetes API responses or control-plane memory as a complete archive.                                                                                 |
| FR-054 | MUST     | The control plane does not depend on executing commands in a terminated container to obtain results.                                                                                                                                       |
| FR-055 | MUST     | A client can list artifacts and obtain on-demand, task-scoped presigned GET URLs while the artifacts are retained.                                                                                                                         |
| FR-056 | MUST     | Artifact metadata includes at least logical path, object reference, size, and media type when known.                                                                                                                                       |
| FR-057 | MUST     | Missing, invalid, oversized, or conflicting result manifests are detected and mapped to a stable task failure category.                                                                                                                    |
| FR-058 | MUST     | Log and artifact presigned GET URLs expire after 15 minutes and can be regenerated while the underlying data is retained.                                                                                                                  |
| FR-059 | MUST     | A task result includes bounded stdout and stderr previews, truncation metadata, byte counts, and storage references for captured streams.                                                                                                  |

### Cancellation and deadlines

| ID     | Strength | Requirement                                                                                                                             |
| ------ | -------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| FR-060 | MUST     | A client can request cancellation of a non-terminal task.                                                                               |
| FR-061 | MUST     | Cancellation is idempotent and records durable cancellation intent before execution resources are changed.                              |
| FR-062 | MUST     | The reconciler eventually stops or releases execution resources for a canceled task.                                                    |
| FR-063 | MUST     | A task that exceeds its execution deadline reaches a distinct timed-out terminal outcome.                                               |
| FR-064 | MUST     | Races among successful completion, failure, cancellation, and timeout have deterministic precedence rules.                              |
| FR-065 | SHOULD   | Partial logs and artifacts that were safely published before cancellation or timeout remain discoverable and are identified as partial. |
| FR-066 | MUST     | A task that remains pending longer than five minutes reaches the `Expired` terminal outcome.                                            |
| FR-067 | MUST     | Sandbox allocation that exceeds two minutes ends as an infrastructure failure.                                                          |

### Reconciliation and cleanup

| ID     | Strength | Requirement                                                                                                                                                                                                |
| ------ | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| FR-070 | MUST     | The service continuously reconciles desired KubeTask task state with durable task data, sandbox or workload state, and result manifests.                                                                   |
| FR-071 | MUST     | Reconciliation is idempotent and safe to retry after partial failure.                                                                                                                                      |
| FR-072 | MUST     | The reconciler detects missing execution resources, stale non-terminal tasks, completed runtimes awaiting finalization, and orphaned resources owned by KubeTask.                                          |
| FR-073 | MUST     | Cleanup state is tracked separately from the task's execution outcome so cleanup failure cannot erase the result.                                                                                          |
| FR-074 | MUST     | Terminal tasks and their stored data follow an operator-configured retention policy. After retained logs or artifacts are deleted, task results report them as expired and do not return stale references. |
| FR-075 | MUST     | Cleanup retries transient failures and exposes persistent cleanup failures to operators.                                                                                                                   |
| FR-076 | MUST     | KubeTask does not reconcile internal lifecycle state owned by the sandbox provider.                                                                                                                        |

### Interfaces

| ID     | Strength | Requirement                                                                                                                            |
| ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| FR-080 | MUST     | The service provides versioned `submit_task`, `run_task`, `get_task`, and `cancel_task` operations and an operation to list artifacts. |
| FR-081 | MUST     | HTTP and MCP interfaces translate to the same application behavior and task model.                                                     |
| FR-082 | MUST     | Public errors use stable machine-readable codes and do not expose internal credentials, stack traces, or raw infrastructure objects.   |
| FR-083 | MUST     | Unknown fields and unsupported protocol versions have documented, deterministic handling.                                              |
| FR-084 | MUST     | The HTTP interface supports request correlation and standard readiness and liveness endpoints.                                         |
| FR-085 | MUST     | Deployed MCP endpoints use Streamable HTTP.                                                                                            |
| FR-086 | MAY      | A stdio MCP transport may be provided for local development.                                                                           |

### Operator configuration

| ID     | Strength | Requirement                                                                                                                                 |
| ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| FR-090 | MUST     | Operators can configure admission limits, runtime image, sandbox template or class, storage location, retention, and reconciliation timing. |
| FR-091 | MUST     | Configuration is validated before the service reports readiness.                                                                            |
| FR-092 | MUST     | Invalid security-critical configuration fails closed.                                                                                       |
| FR-093 | SHOULD   | Configuration reports effective non-secret values for diagnostics.                                                                          |

## Non-functional requirements

### Security

| ID      | Strength | Requirement                                                                                                                                                                                                          |
| ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| NFR-001 | MUST     | The threat model assumes submitted code and input files are malicious.                                                                                                                                               |
| NFR-002 | MUST     | Task workloads comply with the Kubernetes Restricted Pod Security Standard, run as non-root, disallow privilege escalation, drop Linux capabilities, do not use `hostPath`, and do not run as privileged containers. |
| NFR-003 | MUST     | Task workloads receive no Kubernetes service-account token unless a documented runtime integration requires one.                                                                                                     |
| NFR-004 | MUST     | Each task receives only short-lived, task-scoped access to the object keys it must read or write. Long-lived infrastructure credentials are not exposed to user code.                                                |
| NFR-005 | MUST     | Task egress is denied by default. Only DNS and operator-configured infrastructure endpoints required by the runtime are permitted. User code has no general internet or package-repository access.                   |
| NFR-006 | MUST     | CPU, memory, ephemeral storage, process count, process duration, input, output, and log consumption are bounded.                                                                                                     |
| NFR-007 | MUST     | Paths from requests, archives, manifests, and runtime output are normalized and checked for containment before filesystem use.                                                                                       |
| NFR-008 | MUST     | Service and infrastructure credentials are redacted from API errors, logs, metrics, workload metadata, and persisted task status.                                                                                    |
| NFR-009 | MUST     | Kubernetes RBAC and object-store permissions follow least privilege.                                                                                                                                                 |
| NFR-010 | MUST     | Static source scanning, if provided, is treated only as defense in depth and not as the isolation boundary.                                                                                                          |
| NFR-011 | SHOULD   | The runtime supports a configurable Kubernetes `RuntimeClass` for stronger isolation such as gVisor or Kata Containers.                                                                                              |
| NFR-012 | MUST     | A v0.1 deployment restricts HTTP and MCP access to trusted internal callers through platform network controls. Application authentication and authorization are required before public or multi-user deployment.     |
| NFR-013 | MUST     | Source, inputs, stdout, stderr, and artifact contents are not written to operational logs by default.                                                                                                                |
| NFR-014 | MUST     | Each task workload has an enforced process-count limit.                                                                                                                                                              |

### Reliability and consistency

| ID      | Strength | Requirement                                                                                                                                           |
| ------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| NFR-020 | MUST     | Accepted task state survives process restart and loss of a control-plane replica.                                                                     |
| NFR-021 | MUST     | At-least-once reconciliation does not create more than one active execution environment per task.                                                     |
| NFR-022 | MUST     | State updates use concurrency control so stale writers cannot silently overwrite newer state.                                                         |
| NFR-023 | MUST     | Transient Kubernetes, sandbox-provider, persistence, and object-store failures are retried with bounded exponential backoff and jitter.               |
| NFR-024 | MUST     | Permanent failures and exhausted retries become visible through task state or operator signals; they are not retried indefinitely without visibility. |
| NFR-025 | MUST     | The service produces deterministic outcomes for duplicate submissions and repeated cancellation requests.                                             |
| NFR-026 | SHOULD   | The control plane supports multiple replicas without task duplication or split-brain state transitions.                                               |
| NFR-027 | MUST     | Backup and recovery expectations for authoritative task state are documented after the persistence mechanism is selected.                             |

### Performance and capacity

| ID      | Strength | Requirement                                                                                                               |
| ------- | -------- | ------------------------------------------------------------------------------------------------------------------------- |
| NFR-030 | MUST     | API request handling remains independent of user-code execution duration.                                                 |
| NFR-031 | MUST     | The service applies backpressure and rejects excess work rather than creating an unbounded in-memory or Kubernetes queue. |
| NFR-032 | MUST     | Artifact transfer is streamed or chunked so gateway memory usage does not grow linearly with artifact size.               |
| NFR-033 | MUST     | Control-plane CPU and memory consumption are bounded by operator-configurable concurrency and queue limits.               |
| NFR-034 | SHOULD   | The implementation has documented capacity-test results for the supported task concurrency before v0.1 release.           |
| NFR-035 | MUST     | The initial quantitative limits and service objectives are tested before v0.1 is declared production-ready.               |

### Availability and recovery

| ID      | Strength | Requirement                                                                                                                                                   |
| ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| NFR-040 | MUST     | The readiness endpoint reports not ready when the service cannot safely accept tasks because of invalid configuration or loss of an authoritative dependency. |
| NFR-041 | MUST     | Liveness does not depend on the success of an individual task or transient availability of a downstream dependency.                                           |
| NFR-042 | MUST     | After restart, reconciliation resumes incomplete task processing without operator intervention under normal dependency recovery.                              |
| NFR-043 | SHOULD   | Planned control-plane rollout does not terminate running task workloads solely because an API replica stops.                                                  |

### Observability and operability

| ID      | Strength | Requirement                                                                                                                                                                                                         |
| ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| NFR-050 | MUST     | Logs are structured and include internal task ID, correlation ID, component, and state transition where applicable.                                                                                                 |
| NFR-051 | MUST     | Metrics include submitted, accepted, rejected, pending, active, and terminal tasks; queue wait; allocation and execution duration; artifact bytes; reconciliation errors; cleanup failures; and downstream retries. |
| NFR-052 | MUST     | Metrics do not contain unbounded task IDs, external IDs, filenames, or other high-cardinality labels.                                                                                                               |
| NFR-053 | MUST     | Every state transition and rejection can be correlated with an operational log record.                                                                                                                              |
| NFR-054 | MUST     | Prometheus metrics are sufficient for v0.1; distributed tracing is not required.                                                                                                                                    |
| NFR-055 | SHOULD   | Operator documentation covers installation, configuration, dependency failure, stuck-task diagnosis, cancellation, cleanup failure, and recovery.                                                                   |

### Maintainability and compatibility

| ID      | Strength | Requirement                                                                                                                          |
| ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| NFR-060 | MUST     | Control-plane domain types do not expose sandbox-provider, Kubernetes Pod, HTTP, or MCP DTOs as the task domain model.               |
| NFR-061 | MUST     | Task specification and result manifest formats are independently versioned and validated.                                            |
| NFR-062 | MUST     | Runtime and sandbox-provider capabilities are explicit; unsupported behavior is rejected rather than silently degraded.              |
| NFR-063 | MUST     | Behavior, failure cases, state transitions, idempotency, and reconciliation have automated tests.                                    |
| NFR-064 | MUST     | Changes pass formatting, static analysis, unit tests, and relevant integration tests before merge.                                   |
| NFR-065 | SHOULD   | Integration tests cover a real Kubernetes environment, sandbox provider, and S3-compatible object store.                             |
| NFR-066 | MUST     | Meaningful architectural trade-offs are recorded as ADRs before implementation.                                                      |
| NFR-067 | MUST     | Official runtime image dependencies are version-pinned, and the release publishes a package inventory or software bill of materials. |

## Project constraints

These are current design constraints rather than product requirements:

- the control plane is implemented in Go
- the initial task runtime is implemented in Python
- the service runs on Kubernetes
- Agent Sandbox is the only production sandbox lifecycle provider in v0.1
- object storage is S3-compatible
- `llm-sandbox` may be used inside a runtime only where it provides clear value
- KubeTask must not reproduce lifecycle or runtime functionality already owned
  by Agent Sandbox or `llm-sandbox` without a documented reason

Changing a project constraint requires an ADR.

## Initial quantitative limits and service objectives

These values provide an initial reference profile. Operators may configure
stricter limits. Higher limits require validation against cluster and storage
capacity. The values will be reviewed after capacity testing and production
experience.

| Target                                     | Initial v0.1 value                                                  | Rationale                                                                        |
| ------------------------------------------ | ------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| Maximum source and task specification size | 1 MiB combined                                                      | Generous for generated Python code while bounding admission and storage          |
| Maximum client metadata                    | 16 entries; 64 bytes per key; 256 bytes per value; 4 KiB total      | Supports correlation without creating an unbounded metadata surface              |
| Maximum input files                        | 20 files                                                            | Supports common document and data-analysis tasks                                 |
| Maximum input size                         | 50 MiB per file; 100 MiB aggregate                                  | Supports moderate datasets without making KubeTask a large-data transfer service |
| Maximum output files                       | 100 files                                                           | Supports reports, plots, and related files while limiting file-count abuse       |
| Maximum artifact size                      | 50 MiB per file; 250 MiB aggregate                                  | Supports common generated documents and workbooks                                |
| Maximum stdout                             | 1 MiB                                                               | Prevents unbounded output; truncation must be explicit                           |
| Maximum stderr                             | 1 MiB                                                               | Prevents unbounded output; truncation must be explicit                           |
| Maximum inline stdout and stderr preview   | 64 KiB per stream                                                   | Returns common results directly without creating an unbounded API response       |
| Maximum processes per task                 | 256                                                                 | Bounds fork and process exhaustion while supporting common Python libraries      |
| CPU                                        | 100m minimum; 1 CPU default; 2 CPU maximum                          | Provides a practical baseline while limiting initial cluster demand              |
| Memory                                     | 256 MiB minimum; 512 MiB default; 4 GiB maximum                     | Supports common Python tasks and permits larger bounded workloads                |
| Maximum ephemeral workspace                | 1 GiB                                                               | Provides space for inputs, outputs, and temporary files                          |
| Task execution time                        | 1 second minimum; 60 seconds default; 300 seconds maximum           | Supports short-lived tasks while bounding resource use                           |
| Maximum pending time                       | 5 minutes                                                           | Prevents accepted tasks from remaining queued indefinitely                       |
| Maximum sandbox allocation time            | 2 minutes                                                           | Bounds infrastructure allocation delays                                          |
| `run_task` wait time                       | 60 seconds                                                          | Returns fast results directly without coupling the tool call to long execution   |
| Default active task limit                  | 20 tasks per deployment                                             | Provides a conservative initial admission limit, not a scalability ceiling       |
| Default pending task limit                 | 100 tasks per deployment                                            | Absorbs short bursts without creating an unbounded queue                         |
| Submit API latency                         | p95 at most 500 ms; p99 at most 1 second                            | Covers validation, persistence, and durable acceptance, not task execution       |
| Get and cancel API latency                 | p95 at most 250 ms; p99 at most 500 ms                              | These operations use authoritative task state directly                           |
| Reconciliation recovery after restart      | All incomplete tasks reconsidered within 60 seconds after readiness | Provides a measurable recovery objective                                         |
| Task metadata retention                    | 30 days after terminal state                                        | Supports operational diagnosis without indefinite retention                      |
| Log and artifact retention                 | 7 days after terminal state                                         | Provides time for result retrieval while limiting storage growth                 |
| Presigned GET URL lifetime                 | 15 minutes                                                          | Limits bearer-link exposure while allowing immediate result retrieval            |
| Control-plane availability                 | 99.5% per calendar month                                            | Provides an initial objective before production evidence is available            |

The following definitions apply:

- `MiB` and `GiB` are binary units.
- Input and output limits apply to uncompressed bytes.
- Truncated stdout or stderr includes an explicit truncation indicator. The
  preview preserves content from the beginning and end of the stream.
- A task result reports whether each stream was truncated, the captured and
  total byte counts, and a storage reference for the bounded captured stream.
- The runtime continues to drain stdout and stderr after reaching capture limits
  so user code cannot block on a full pipe.
- Task execution time starts when the runtime begins workspace preparation. It
  includes input retrieval, user-code execution, artifact collection, and result
  publication. It excludes sandbox allocation.
- API latency is measured at the service boundary and excludes task dispatch
  and execution.
- Active and pending task limits are default admission settings, not tested
  platform capacity.
- Retention starts when a task enters a terminal state.
- Control-plane availability is the percentage of valid API requests that do
  not fail because of KubeTask or a required dependency.

## Product and policy decisions

| Area                 | v0.1 decision                                                                                                                                            |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Initial clients      | OpenWebUI, Archestra.ai, and other backend services or agents that orchestrate LLM requests                                                              |
| Interfaces           | MCP over Streamable HTTP is the primary agent-facing interface; HTTP exposes the same task operations to backend services                                |
| Deployment trust     | The service is deployed on a trusted internal network, and platform network controls restrict access                                                     |
| Authentication       | Application-level authentication is not required for v0.1                                                                                                |
| Authorization        | Task ownership enforcement is not required for v0.1                                                                                                      |
| Tenancy              | The service is single-tenant                                                                                                                             |
| Future tenancy       | Bounded client metadata supports correlation but is never used as verified identity, ownership, or quota scope                                           |
| Execution backend    | Agent Sandbox is the only production execution backend                                                                                                   |
| Source submission    | The task request contains inline UTF-8 Python source                                                                                                     |
| Source delivery      | The control plane persists source and gives the runtime a task-scoped storage reference                                                                  |
| Resource controls    | Clients may request only an execution timeout within configured bounds; CPU, memory, workspace, and process limits are operator-controlled               |
| Input files          | Input references contain a relative path and object key in one configured S3 bucket and prefix; arbitrary URLs and inline base64 files are not supported |
| Python dependencies  | The official runtime image includes NumPy, pandas, SciPy, Matplotlib, Seaborn, Pillow, openpyxl, XlsxWriter, python-docx, pypdf, and ReportLab           |
| Dynamic installation | Runtime package installation is not supported                                                                                                            |
| Network access       | User code has no general internet or package-repository access                                                                                           |
| Execution attempts   | Allocation and preparation may retry; user code runs at most once after the task enters `Running`                                                        |
| Output collection    | The runtime recursively collects regular files from `/workspace/outputs`; the API does not accept output glob patterns                                   |
| Output limit failure | Exceeding an output limit fails the task and preserves bounded logs and uploaded artifacts as partial results                                            |
| Logs                 | Results contain bounded stdout and stderr previews and storage references for bounded captured streams                                                   |
| Artifact access      | Clients obtain on-demand presigned GET URLs that expire after 15 minutes                                                                                 |

Client metadata has the following rules:

- key count and key and value sizes are bounded
- metadata is stored with the task; operational logs include keys and only
  operator-allowlisted values
- metadata is not used as a Prometheus label
- metadata is not propagated directly to Kubernetes labels
- metadata never establishes identity, ownership, authorization, or quota scope

If multi-tenancy is added later, authenticated tenant and principal identifiers
must be derived by the service and kept separate from client metadata.
