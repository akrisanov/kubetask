# KubeTask

KubeTask will be a Kubernetes-based execution service for isolated, short-lived
tasks. It will handle task orchestration and lifecycle management, including
resource controls, artifact storage, status tracking, and cleanup.

## Project status

KubeTask is in the design stage. The repository contains no implementation
code, API, binary, container image, or release.

The initial design uses a Go control plane and a Python task runtime. It relies
on existing projects such as
[llm-sandbox](https://github.com/vndee/llm-sandbox) and
[Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) for runtime
and sandbox capabilities.

Design decisions will be recorded in [`docs/ADR`](docs/ADR) as the project
develops.

## License

KubeTask is licensed under the terms in [LICENSE](LICENSE).
