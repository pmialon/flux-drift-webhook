# Development

How to build, test and run `flux-drift-webhook` locally. The conventions here mirror the FluxCD
community's.

## Prerequisites
- **Go 1.26.5+**
- **kustomize 5.4+** and **kubectl**
- **Docker** or **Podman** (for image builds / e2e)
- **golangci-lint v2** (for `make lint`)
- For e2e: **kind** and a local cert-manager manifest at `e2e/cert-manager.yaml`

The manager is bootstrapped on [`fluxcd/pkg/runtime`](https://pkg.go.dev/github.com/fluxcd/pkg/runtime)
(logger, leader election, events, pprof and probes option structs) with flags bound via `pflag`.

## Common tasks
```sh
make build           # build the webhook binary into bin/
make test            # unit tests (CGO-free); also runs the native fuzz seed corpora; writes coverage.out
make test-unit       # short unit tests only (-short, CGO-free)
make test-race       # unit tests with the race detector (requires CGO)
make fuzz-smoketest  # run each native Go fuzz target for FUZZ_TIME (default 20s)
make envtest         # install the kube-apiserver/etcd binaries for the integration suite
make lint            # golangci-lint
make fmt vet tidy    # format, vet, and tidy go.mod (-compat=1.26)
make verify          # full local gate: fmt+vet+tidy+generate+lint+build+test+manifests + clean-tree check
make verify-docker   # build the image and smoke-test its --help entrypoint
make ci              # local full-gate aggregate: verify + verify-docker + test-integration + fuzz-smoketest
```

## Tests
- Unit tests use the standard `testing` package with table-driven cases. Run a single test with:
  ```sh
  go test ./internal/webhook/ -run TestHandle_DeleteInheritedLabelAllowed -v
  ```
- Integration tests are behind a build tag (`//go:build integration`): `make test-integration`. They
  run against a **real apiserver** via `fluxcd/pkg/runtime/testenv`, so they depend on `make envtest`,
  which installs the `kube-apiserver`/`etcd` binaries through `setup-envtest` (exporting
  `KUBEBUILDER_ASSETS`, `ENVTEST_K8S_VERSION=1.36.0` with a `1.35.0` fallback).
  > **Note:** those binary assets are fetched from an external GitHub index, **not** via
  > `GOPROXY`; GitHub-hosted runners can fetch them over the network, so in CI `test-integration`
  > runs as a **required automatic gate**. Fallbacks in restricted networks: pre-seed the assets
  > (`ENVTEST_INSTALLED_ONLY=1`) or pass `--index <mirror>`.
- Webhook integration against a live cluster (audit-only): `make test-webhook`.
- Full end-to-end on a kind cluster: `make test-e2e`.

### CI gates
On top of `make verify`, the pipeline adds `fuzz-smoketest`, `verify-codegen`, and the
`test-integration` envtest gate. Locally, `make ci`
is the full-gate aggregate (`verify` + `verify-docker` + `test-integration` + `fuzz-smoketest`).

## Helm chart
A Helm chart in [`charts/flux-drift-webhook/`](charts/flux-drift-webhook) is an alternative to the
Kustomize manifests in `deploy/`. Lint and render it locally:
```sh
helm lint charts/flux-drift-webhook
helm template flux-drift-webhook charts/flux-drift-webhook -n flux-system | kubeconform -strict -ignore-missing-schemas -summary
```
GitHub Actions lints the chart on every pull request (the `helm` job) and publishes it to
`oci://ghcr.io/pmialon/charts/flux-drift-webhook` on `vX.Y.Z` tags (the release workflow). Keep the
templates in sync with `deploy/base` (the chart mirrors it 1:1).

## Running locally
The webhook needs TLS certificates and a reachable API server. The simplest path is the kind-based
flow in `e2e/run-e2e.sh`, which installs cert-manager, deploys the webhook and runs the integration
tests. See [`CLAUDE.md`](CLAUDE.md) for the architecture and configuration flags.

## Before opening a pull request
Run `make verify` — it must pass and leave a clean git tree (no uncommitted codegen/tidy/format
changes). See [`CONTRIBUTING.md`](CONTRIBUTING.md).
