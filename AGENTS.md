# Guidance for AI coding assistants

This repository follows the FluxCD community's engineering conventions. If you are an AI agent
contributing here, read this file first.

## Golden rules
- **You (the AI) MUST NOT add `Signed-off-by:` or `Co-authored-by:` trailers.** Only a human can
  certify the [Developer Certificate of Origin](https://developercertificate.org/). The human
  author signs off (`git commit -s`); you do not.
- **Disclose AI assistance** with an `Assisted-by:` trailer instead:

  ```sh
  git commit -s -m "Short imperative subject" --trailer "Assisted-by: <agent>/<model>"
  ```
- The human author is **responsible for understanding** every line you submit. Keep changes
  minimal, reviewable and free of verbose AI boilerplate.

## Build, test and lint
```sh
make build           # compile ./cmd/webhook
make test            # CGO-free unit tests with coverage (also runs the fuzz seed corpora)
make test-race       # tests with the race detector (needs CGO)
make test-integration # envtest integration suite (real apiserver; needs envtest assets — see DEVELOPMENT.md)
make fuzz-smoketest  # smoke-test the native Go fuzz targets
make lint            # golangci-lint (config: .golangci.yml)
make fmt vet tidy    # format, vet, tidy (go.mod -compat=1.26)
make verify          # fmt+vet+tidy+generate+lint+build+test+manifests, then a clean-tree check
make ci              # full local gate (verify + verify-docker + test-integration + fuzz-smoketest)
```
`make verify` must pass and leave a **clean git tree** before you open a pull request.

## Conventions
- Every `.go` file carries the Apache-2.0 header from `hack/boilerplate.go.txt`.
- Document every exported identifier; follow
  [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).
- Preserve backward compatibility of public behaviour and admission decisions; add regression tests.

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full contribution process and
[`DEVELOPMENT.md`](DEVELOPMENT.md) for the local workflow.
