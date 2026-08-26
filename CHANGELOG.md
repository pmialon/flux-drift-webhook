# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[SemVer](https://semver.org/) (0.x: minor versions may carry breaking changes —
read the **Breaking / upgrade notes** before widening a version range).

## [Unreleased]

Nothing yet.

## [0.3.0] - 2026-08-26

The full remediation of the 2026-07-31 external review: all 41 findings
addressed (admission-logic hardening, VWC lifecycle, CI/supply chain,
observability, chart, tests, docs).

### Breaking / upgrade notes

- The Helm chart now **refuses to render** when the release namespace differs
  from `config.fluxNamespace` (default `flux-system`). Installs outside the
  Flux namespace were silently broken (the webhook would deny the Flux
  controllers themselves in enforce mode); set `config.fluxNamespace`
  explicitly only if Flux itself runs in that namespace.
- `values.schema.json` now rejects unknown keys at the root and under
  `config`. Typoed values that were silently ignored (e.g. `config.audit_only`)
  now fail `helm install`. Fix the key names.
- The multi-tenant fallback identity (`flux-reconciler`) is only accepted in
  the **owning** Kustomization/HelmRelease's namespace. Setups relying on a
  reconciler SA in any other namespace were relying on a spoofable check.
- The release workflow's `workflow_dispatch` dry-run trigger was removed (it
  never worked).
- The unreachable decision reasons `denied_diff_error` and
  `denied_internal_error` were removed; `denied_metadata_error` is new (and,
  unlike before, honours `--audit-only`).

### Added

- The deployed manifests ship the real (static) VWC rules and matchConditions;
  the controller applies the safety-critical fields (`failurePolicy`,
  `timeoutSeconds`, namespaceSelector) and recreates a deleted VWC fail-open,
  with a `ConfigRecreated` Warning Event (needs the new `create` RBAC verb,
  granted by base and chart).
- Alerts: a `PrometheusRule` (base + chart, `prometheusRule.enabled`) covering
  the fail-open window, VWC re-apply failures, deny spikes and ownership
  conflicts; the NetworkPolicy now admits the metrics/health ports
  (`networkPolicy.metricsFrom` restricts peers).
- Chart knobs: `webhook.failurePolicy` / `webhook.timeoutSeconds` /
  `webhook.extraExcludedNamespaces` (driven through new binary flags so both
  VWC writers stay identical), `priorityClassName`,
  `topologySpreadConstraints`, `certManager.certificate.duration`/`renewBefore`.
- Cache diet for large estates: owners cached with only the three read fields,
  Namespaces watched metadata-only, managedFields stripped everywhere; a
  "Resource Sizing" README section documents scaling the memory limit.
- New metric `flux_drift_webhook_owner_lookup_errors_total{owner_kind}`;
  latency buckets extended to 10s to cover the timeout region.
- CI: the kind e2e suite runs on every PR; releases are gated on tests; a
  weekly Trivy scan re-examines the published image; `govulncheck` runs per
  PR; the envtest suite is matrixed over Kubernetes 1.30 (the floor) and 1.36.
- e2e: the HelmRelease half (H1–H4, T6h) and a VWC auto-repair test (T13).

### Fixed

- `rules: []` in the shipped manifests let a GitOps applier wipe the
  controller-applied VWC rules on every reconciliation (protection silently
  OFF up to ~50% of the time).
- A deleted VWC was recreated with apiserver defaults (`failurePolicy: Fail`,
  no namespaceSelector, no caBundle): a cluster-wide fail-closed trap.
- A SA named `apiserver` in a namespace named `system` could spoof
  `system:apiserver` and delete Flux-applied resources.
- RBAC least privilege: VWC writes scoped to the managed object, leases moved
  to a namespaced Role (previously: write access to **every** VWC and lease in
  the cluster).
- `helm test` could never pass (runAsNonRoot with a root busybox image).
- `make deploy-dev`/`deploy-prod` deployed a floating `:latest`; both overlays
  now pin the released tag.
- Metadata-extraction failures hard-denied even in audit mode and were
  invisible in metrics.
- A leftover `--discovery-interval` silently overrode an explicit
  `--vwc-resync-interval`.
- `--system-controller-sas` accepted silently-inert entries; now validated at
  startup.
- Docs: chart README floor/runtime defaults, NOTES.txt discovery wording,
  metrics port-forward command, emergency-disable runbook.

## [0.2.0] - 2026-07-23

### Breaking / upgrade notes

- **Kubernetes floor raised to 1.30** (CEL `matchConditions` GA); the chart
  declares `kubeVersion: ">=1.30.0-0"`.
- `--discovery-interval` is deprecated (alias of `--vwc-resync-interval`):
  per-GroupVersion discovery was replaced by a single wildcard rule + CEL
  matchConditions, covering CRDs the moment they are installed.
- The prod overlay switched from audit-only to **enforce**.

### Added

- Enforce-mode e2e suite (E1–E13) proving the webhook blocks, on a vendored,
  offline-capable kind stack (cert-manager, Flux, podinfo).
- `.spec.ignore` (Flux DriftIgnoreRules) waiver, owner-inventory CREATE veto,
  multi-tenant identity via the owner's `.spec.serviceAccountName`,
  namespace-teardown cascade bypass, readiness gated on informer cache sync.
- Ownership-conflict metric (`flux_drift_webhook_ownership_conflicts_total`).

## [0.1.0] - 2026-07-23

Initial release: field-level drift prevention for FluxCD-managed resources
(SSA managedFields-based), audit and enforce modes, Kustomize manifests and
Helm chart, cosign-signed multi-arch images with SBOM and SLSA provenance.

[Unreleased]: https://github.com/pmialon/flux-drift-webhook/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/pmialon/flux-drift-webhook/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/pmialon/flux-drift-webhook/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/pmialon/flux-drift-webhook/releases/tag/v0.1.0
