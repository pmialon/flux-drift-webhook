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

## Read this before changing behaviour

[`CLAUDE.md`](CLAUDE.md) is the reference for the admission decision paths, the
bypass ordering, the decision-label contract and every documented invariant —
read the relevant section **before** touching `internal/webhook` or
`internal/controller`, and update it in the same change when behaviour moves.

## Build, test and lint
```sh
make verify          # fmt+vet+tidy+generate+lint+build+test+manifests, then a clean-tree check
make ci              # full local gate (verify + verify-docker + test-integration + fuzz-smoketest + test-e2e)
```
`make verify` must pass and leave a **clean git tree** before you open a pull request.
The full target list lives in [`DEVELOPMENT.md`](DEVELOPMENT.md) and `Makefile`.

## Conventions
- Every `.go` file carries the Apache-2.0 header from `hack/boilerplate.go.txt`.
- Document every exported identifier; follow
  [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).
- Preserve backward compatibility of public behaviour and admission decisions; add regression tests.

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full contribution process and
[`DEVELOPMENT.md`](DEVELOPMENT.md) for the local workflow.
