# Contributing

`flux-drift-webhook` adopts the [FluxCD](https://github.com/fluxcd) community's contribution
conventions so the project can hold the same Go-quality bar.

## Developer Certificate of Origin (DCO)
Every commit MUST be signed off, certifying the [DCO](https://developercertificate.org/):

```sh
git commit -s -m "Short imperative subject"
```

This adds a `Signed-off-by: Your Name <you@example.com>` trailer. Only a human may sign off.
AI assistants MUST NOT add `Signed-off-by` or `Co-authored-by` — they disclose with an
`Assisted-by: <agent>/<model>` trailer instead (see [`AGENTS.md`](AGENTS.md)).

## Commit messages
- Follow [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): subject`,
  where `type` is one of `feat`, `fix`, `docs`, `refactor`, `test`, `chore`.
- Subject: imperative mood, ≤ 50 characters, no trailing period ("add" not "added").
- Blank line, then a body wrapped at 72 characters explaining **what** and **why**.
- No issue/PR @mentions in the message; reference them in the pull-request description.

## Code conventions
- Follow [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).
- Document every exported identifier; keep functions small and focused.
- Every `.go` file carries the Apache-2.0 header (`hack/boilerplate.go.txt`).
- Add tests for new behaviour; preserve existing admission decisions and metrics (regression tests).

## Before you submit
```sh
make verify   # fmt + vet + tidy + generate + lint + build + test + manifests + clean-tree check
```
`make verify` must pass with a clean working tree. GitHub Actions additionally gates on
`fuzz-smoketest`, `verify-codegen` and the envtest `test-integration` suite (hosted runners have the
network access to fetch the envtest assets). `make ci` runs the full local gate. See
[`DEVELOPMENT.md`](DEVELOPMENT.md) for the local workflow.

## Pull request process
1. Branch from `main` (direct pushes to `main` are not allowed).
2. Make focused commits (signed off).
3. Open a pull request; ensure CI is green.
4. Address review feedback; squash and rebase on `main` before merge (no merge commits).
