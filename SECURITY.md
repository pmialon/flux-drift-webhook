# Security Policy

## Reporting a Vulnerability

Please report security vulnerabilities **privately** — do not open a public issue.

- Preferred: open a private advisory via GitHub Security Advisories at
  <https://github.com/pmialon/flux-drift-webhook/security/advisories/new>.
- Alternatively, email the maintainer at <pmialon@gmail.com>.

Include enough detail to reproduce the issue: the affected version or image tag, the Kubernetes
version, the webhook mode (audit or enforce) and a description of the impact. We will acknowledge
your report, work on a fix and coordinate a disclosure timeline with you. Reporters who wish to be
credited will be acknowledged.

## Supported Versions

This project is pre-1.0; only the latest released version receives security fixes.

## Verifying release artefacts

Container images are published to `ghcr.io/pmialon/flux-drift-webhook` and signed with
[cosign](https://github.com/sigstore/cosign) using keyless (GitHub OIDC) signatures. Verify a
release image with:

```sh
cosign verify \
  --certificate-identity-regexp "^https://github.com/pmialon/flux-drift-webhook/.github/workflows/release.yaml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  ghcr.io/pmialon/flux-drift-webhook:<tag>
```
