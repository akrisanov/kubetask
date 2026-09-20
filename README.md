# KubeTask

KubeTask is a Kubernetes-based execution service for isolated, short-lived tasks.
It handles task orchestration and lifecycle management, including resource controls,
artifact storage, status tracking, and cleanup.

## Project status

The repository currently implements the dependency-free task lifecycle domain
model from [ADR 0002](docs/ADR/0002-task-lifecycle.md).
It has no persistence, API, executor, or Kubernetes integration yet.

The initial design uses a Go control plane and a Python task runtime.
It relies on existing projects such as [llm-sandbox](https://github.com/vndee/llm-sandbox) and
[Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) for runtime
and sandbox capabilities.

Design decisions are recorded in [`docs/ADR`](docs/ADR) as the project develops.

## Development tools

Development requires Go 1.27.0 or later, as declared in [`go.mod`](go.mod), and
[`govulncheck`](https://go.dev/blog/govulncheck).

Install `govulncheck` with:

```sh
go install golang.org/x/vuln/cmd/govulncheck@latest
```

## Checks

Run the domain checks with:

```sh
gofmt -w internal/task/*.go
go vet ./...
go test ./...
govulncheck ./...
```

## License

KubeTask is licensed under the terms in [LICENSE](LICENSE).

---

(C) 2026, Andrey Krisanov 🍁
