# Deployora

Test a Kubernetes deployment on a throwaway cluster before it reaches production.

> **Status: early development.** The design is written and the core packages exist, but `deployora run` does not work yet. Nothing here is ready to use. Star or watch the repository to follow progress.

## What it will do

Deployora answers one question: *"Will this deploy and run healthy on a clean, throwaway Kubernetes cluster, and if not, why?"*

Given a Helm chart (or Kubernetes manifests), it will:

1. validate it with kubeconform,
2. create a disposable Kind cluster,
3. deploy it and run readiness and service checks,
4. collect redacted evidence and write JSON and HTML reports,
5. tear everything down, even when the run is interrupted.

The goal for v0.1.0 is a single command with no configuration file:

    deployora run ./chart

It is meant for Helm chart maintainers and platform engineers. It complements Helm, chart-testing (`ct`), Chainsaw and kubeconform; it does not replace them.

## Design

- [Architecture overview](docs/architecture/overview.md): the planned design and its rules.
- [Kind interrupt spike](docs/spikes/kind-interrupt.md): what we learned about cancelling and cleaning up a Kind cluster.

## Development

You need Go 1.25.5 or later.

    go vet ./...
    go test ./...

## Contributing

Contribution guidelines are coming. Until then, please open an issue to discuss an idea before sending a pull request.

## License

Apache License 2.0. See [LICENSE](LICENSE).