# KubeTask Agents

## General rules

- Before making architectural or substantial implementation changes, read
  `docs/requirements.md` and the relevant accepted ADRs in `docs/ADR/`.
- Keep KubeTask focused on receiving LLM-generated code from external
  applications, executing it in isolation, managing inputs and artifacts,
  and enforcing resource and lifecycle limits.
- Keep model selection, LLM calls, planning, tool selection, and agent loops
  in the calling application. KubeTask must not require an LLM to operate.
- Do not expand KubeTask into an agent execution or orchestration platform.
- Do not recreate functionality already provided by llm-sandbox or Agent Sandbox
  without a documented reason.
- Prefer explicit state transitions and reconciliation over request-scoped
  lifecycle logic.
- Do not expand the v0.1 scope without discussing the change first.
- Preserve the control-plane/runtime boundary defined in `docs/requirements.md`
  and the accepted ADRs in `docs/ADR/`.
- For architectural decisions with meaningful trade-offs, propose an ADR
  before implementation.
- Add tests for behavior, failure cases, and state transitions.
- Run the relevant test and lint commands after changes.

## Working with Git

- Use the conventional commit format for commit messages.

## Go implementation guidelines

- Use the Go version declared in `go.mod` and format code with `gofmt`.
  Prefer standard-library solutions when they meet the requirements.
- Keep packages cohesive and exported APIs small. Define small interfaces
  where they are consumed when a concrete dependency boundary needs one.
  Avoid speculative abstractions and generic utility packages.
- Pass `context.Context` explicitly as the first parameter for operations
  that can block. Propagate cancellation and deadlines, and release
  resources associated with derived contexts.
- Use request contexts for request work and service-owned contexts for
  reconciliation. An accepted task must survive client disconnection.
- Every goroutine must have an owner, a termination condition, and a way
  for its owner to wait for it. Bound concurrency and queues, and document
  ownership of shared mutable state.
- Return errors for expected failures. Add useful operation context,
  preserve underlying errors with `%w` when callers need to inspect them,
  and use `errors.Is` or `errors.As` instead of matching error strings.
- Handle errors at the layer that can act on them. Avoid logging the same
  failure at every layer. Keep process exit decisions in the entry point.
- Close files, response bodies, and other resources promptly. Handle
  write, flush, and close errors when they affect data integrity.
- Set explicit network timeouts and input limits. Stream potentially large
  payloads through enforced size limits instead of reading them unbounded.
- Retry only operations that are safe to repeat and failures that may be
  transient. Bound individual retry loops, use backoff with jitter, honor
  cancellation, and account for retries already performed by clients or
  controllers. Keep unresolved durable work eligible for later reconciliation
  under the task lifecycle and cleanup rules.
- Validate configuration at startup and inject dependencies explicitly.
  Avoid mutable global state and hidden initialization side effects.
- Use structured logs with stable fields. Do not log submitted source,
  file contents, credentials, or presigned URLs. Keep metric labels bounded.
- Implement bounded graceful shutdown that stops admission and drains or
  cancels process-owned work while preserving durable task state for recovery.
  Control-plane shutdown must not itself cancel accepted tasks or terminate
  their runtime workloads.
- Test observable behavior, failure paths, cancellation, duplicate delivery,
  and concurrent state transitions. Use explicit synchronization instead
  of sleeps to coordinate tests.
- Fuzz parsers and validation boundaries that process untrusted input.
  Benchmark before introducing performance-driven complexity.
- After Go changes, run `go test ./...` and `go vet ./...`.
- Run `go test -race ./...` for concurrency changes and in CI.
- Run `govulncheck ./...` in CI and after dependency changes.
- Run integration tests when changing Kubernetes or storage behavior.
- Report checks that could not run and the reason.

## Go code style and readability

- Follow idiomatic Go naming and use `gofmt`. Avoid redundant names such
  as `task.TaskManager` when `task.Manager` conveys the same meaning.
- Use short names in small scopes and descriptive names across longer
  scopes. Preserve standard initialisms such as ID, HTTP, and URL.
- Prefer straightforward control flow. Handle errors and invalid cases
  early, and keep the successful path minimally nested.
- Keep functions focused on one coherent operation. Extract helpers when
  they clarify intent or isolate meaningful behavior. Avoid arbitrary
  function-length limits and unnecessary one-line wrappers.
- Keep related code together and organize files by responsibility.
  Avoid catch-all packages or files named `utils`, `helpers`, or `common`.
- Prefer concrete types and explicit data flow. Use generics, reflection,
  or `any` only when they solve a clear problem more simply.
- Use named types and constants for domain concepts and states.
  Use `time.Duration` for durations and make units explicit at boundaries.
- Make ownership and mutation clear. Document whether methods mutate
  receivers, retain supplied data, or are safe for concurrent use.
- Write comments that explain intent, constraints, invariants, or
  non-obvious behavior. Avoid comments that merely restate the code.
- Document exported APIs and important internal contracts, including
  cancellation behavior, partial results, and meaningful error conditions.
- Keep tests readable as behavioral examples. Use descriptive case names,
  explicit inputs and expectations, and failure messages with useful context.
- Follow established local conventions unless changing them is part of
  the task. Keep unrelated formatting and naming changes out of patches.
