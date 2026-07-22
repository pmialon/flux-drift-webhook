# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This repository contains **flux-drift-webhook**, a Kubernetes Validating Admission Webhook that prevents manual drift on FluxCD-managed resources. It is inspired by Google Config Sync's drift prevention feature.

The webhook dynamically discovers all Kubernetes API GroupVersions and protects resources labelled by:
- **Kustomization**: `kustomize.toolkit.fluxcd.io/name` and `kustomize.toolkit.fluxcd.io/namespace`
- **HelmRelease**: `helm.toolkit.fluxcd.io/name` and `helm.toolkit.fluxcd.io/namespace`

## Repository Structure

```
flux-drift-webhook/
├── cmd/webhook/main.go                 # Entry point (fluxcd/pkg/runtime option structs + controller-runtime)
├── internal/
│   ├── config/config.go                # Constants (labels, annotations, ports)
│   ├── controller/
│   │   ├── webhook_config.go                       # Controller updating ValidatingWebhookConfiguration
│   │   ├── suite_test.go                           # envtest suite bootstrap (build tag: integration)
│   │   └── webhook_config_integration_test.go      # envtest integration tests (build tag: integration)
│   ├── discovery/discovery.go          # GVK discovery via Kubernetes API
│   ├── metrics/metrics.go              # Prometheus metrics
│   └── webhook/
│       ├── handler.go                  # Main admission logic
│       ├── fields.go                   # SSA managedFields extraction and diff
│       ├── auth.go                     # Flux service account detection
│       ├── labels.go                   # Flux labels detection
│       └── fuzz_test.go                # Native Go fuzz targets
├── deploy/
│   ├── base/                           # Kustomize base manifests (incl. PDB)
│   └── overlays/
│       ├── dev/                        # Audit-only mode
│       └── prod/                       # Enforce mode
├── e2e/
│   ├── run-e2e.sh                      # E2E test script (kind cluster)
│   └── test-webhook.sh                 # Integration tests against live webhook
├── .github/workflows/                  # CI and release (GitHub Actions)
├── .golangci.yml                       # golangci-lint configuration
├── Dockerfile
├── Makefile
└── go.mod
```

## Build Commands

```bash
# Build binary
make build

# Run all tests (CGO-free, works on any host); also runs the fuzz seed corpora
make test

# Run tests with race detector (requires CGO)
make test-race

# Run unit tests only
make test-unit

# Generate the HTML coverage report (runs `make test` first)
make coverage

# Run webhook integration tests against a live cluster
make test-webhook

# Install envtest binaries into ./bin and print the asset directory
make envtest

# Run the envtest integration suite (build tag `integration`; sets KUBEBUILDER_ASSETS
# via setup-envtest — the binary assets download from an external GitHub index
# over the network; works on CI runners and workstations alike).
make test-integration

# Run full E2E tests (kind cluster + deploy + integration tests)
make test-e2e

# Smoke-test every native Go fuzz target (each runs for FUZZ_TIME, default 20s)
make fuzz-smoketest

# Run linter (golangci-lint; gosec runs via the gosec linter)
make lint

# Format, vet, and tidy (tidy uses `-compat=1.26`)
make fmt
make vet
make tidy

# Build Docker image
make docker-build IMG=<registry>/flux-drift-webhook:<tag>

# Push Docker image
make docker-push IMG=<registry>/flux-drift-webhook:<tag>

# Deploy to dev (audit-only mode)
make deploy-dev

# Deploy to prod (enforce mode)
make deploy-prod

# Aggregate verification: fmt vet tidy generate lint verify-structure
# verify-build test verify-manifests verify-dirty
make verify

# Fail if fmt/vet/tidy/generate left the working tree dirty
make verify-dirty

# Full local CI pipeline (verify + verify-docker + test-integration + fuzz-smoketest)
make ci

# Clean build artefacts
make clean
```

## Architecture

### Protection Logic

| Operation | Flux-managed Resource | Result |
|-----------|----------------------|--------|
| DELETE | Yes (Flux SSA fieldManager present) | DENY (unless Flux controller, bypass or `reconcile: disabled` annotation, a recognised control-plane controller — GC cascade, Job TTL/CronJob cleanup, CRD-instance cleanup by `system:apiserver` — or the parent **namespace is terminating**). A resource carrying Flux labels only by **inheritance** (no Flux fieldManager) is ALLOWED. |
| UPDATE | Yes | **Field-level check** (see below) — evaluated against the **old** object (labels and managedFields), hierarchy-aware, with managedFields-tampering and bypass-introduction guards |
| CREATE | With Flux labels | The **owner inventory is authoritative** when readable: id present → DENY for every non-Flux actor; id absent → ALLOW (derived object). Fallback heuristics when unreadable: controller `ownerReference` or recognised system controller, else fail-closed (`denied_create_inventory_unavailable`) |

Applies to **all scopes** (VWC `Scope: "*"`): cluster-scoped Flux-managed objects (Namespaces, CRDs, ClusterRoles, PVs, …) are protected too.

### Label Precedence

When a resource has both Kustomization and HelmRelease labels, **Kustomization takes precedence**. In practice, a resource should be owned by exactly one Flux controller, but this deterministic ordering avoids ambiguity.

### Field-Level Protection (SSA ManagedFields)

For UPDATE operations, the webhook uses **Server-Side Apply (SSA) managedFields** to determine which fields are managed by Flux. This allows controllers like HPA, VPA, and KEDA to modify fields they own without being blocked.

**How it works:**
1. Extract Flux-managed fields from the **OLD** object's `.metadata.managedFields` (entries where `manager` matches `kustomize-controller` or `helm-controller`, with optional version suffix). The old object is authoritative: the API server transfers field ownership to the requester **before** validating admission runs, so on the new object drifted fields are no longer attributed to Flux.
2. Compute the actual value diff between old and new objects (only fields with changed values)
3. Check if modified fields overlap with Flux-managed fields — **hierarchy-aware**: a modified path that is an ancestor or descendant of a Flux-managed path conflicts. This is what connects the schema-blind diff (which records a keyed-list edit as the whole list path, e.g. `.spec.template.spec.containers`) to Flux's fieldsV1 (which records members *inside* the list, `k:{"name":...}`). Trade-off: any change inside a keyed list Flux partially owns counts as a conflict, even for entries Flux does not declare (conservative deny; the bypass annotation is the escape hatch).
4. **ALLOW** if no overlap (e.g., HPA updating `.spec.replicas` when Flux doesn't manage it)
5. **DENY** if overlap exists (user trying to modify a Flux-managed field)
6. **DENY** (`denied_managed_fields_tampered`) if the Flux entry shrank between old and new without any overlapping value change — e.g. wiping `.metadata.managedFields` or SSA-applying a reduced config under the `kustomize-`/`helm-controller` manager name, which would silently disarm the field check for every later request. Legitimate ownership transfers always accompany a value change, which step 5 already catches.

The management gate also evaluates the **old** object's labels on UPDATE: stripping the Flux labels and drifting the spec in a single request is denied (the label removal itself conflicts with the Flux-owned label fields).

7. **WAIVE** conflicting fields excluded from drift detection by the owning Kustomization's `.spec.ignore` (Flux v2.9 DriftIgnoreRules). Flux keeps managedFields ownership of a sole-owned ignored field (`driftAdopt`), so step 5 would still deny an edit to it even though Flux itself no longer corrects it. See the `allowed_drift_ignored_field` bypass (3c) below. The same ignore set is applied to the step-6 tampering check so a legitimate ignored-field edit (which transfers that field out of Flux's managedFields) is not mistaken for tampering.

**Example:**
```
Flux manages: spec.template, metadata.labels
HPA manages:  spec.replicas

→ HPA can freely update spec.replicas
→ User cannot modify spec.template
```

### Bypass Mechanisms

Checked in this order:

1. **Sub-resource requests** (any operation): requests for a sub-resource (`status`, `scale`, …) are allowed — they do not carry the parent object needed for drift evaluation and the VWC rules do not select them (`allowed_subresource`).
2. **Owning Flux controller**: Requests from `kustomize-controller`, `helm-controller`, etc. in `flux-system` namespace — only if they are the controller that owns the resource (labels unchanged). If a different Flux controller tries to modify the resource, it is denied (`denied_wrong_flux_controller`). In **multi-tenant mode**, Flux reconciles tenant resources by impersonating a per-tenant service account. The webhook resolves the legitimate identity dynamically: it reads `.spec.serviceAccountName` from the owning Kustomization/HelmRelease (looked up via the labels) and only accepts `system:serviceaccount:<owner-ns>:<that-sa>`. If the owner cannot be read or sets no service account, it falls back to the static names in `config.FluxReconcilerServiceAccounts` (default `flux-reconciler`). Owner lookups use the controller-runtime cache (requires get/list/watch RBAC on kustomizations and helmreleases).
3. **Bypass annotation**: `fluxcd.io/drift-prevention-bypass: disabled` — must already exist on the **old** object (applied via Git/Flux). Never honoured on **CREATE** (there is no pre-request state proving Git provenance — otherwise including it in the created object would defeat the CREATE protection, inventory veto included). Adding the annotation in the same UPDATE request is blocked to prevent single-step bypass attacks, and a non-Flux UPDATE **introducing** a protection-disabling annotation (this one or `reconcile: disabled`) on a Flux-applied object is denied outright (`denied_bypass_annotation_added`). Inherited-label objects (no Flux fieldManager) are not affected by this guard.
3b. **Reconcile disabled** (Kustomization-owned, UPDATE/DELETE): `kustomize.toolkit.fluxcd.io/reconcile: disabled` on the **old** object is honoured as a bypass (`allowed_reconcile_disabled`) — kustomize-controller skips such objects entirely, so drift prevention on them would be incoherent (Flux neither corrects nor reapplies). helm-controller has no per-object equivalent, so HelmRelease-owned objects are unaffected. Same Git-only introduction rule as the bypass annotation.
3c. **Drift-ignored fields** (Kustomization-owned, UPDATE): a modified field that overlaps the Flux-managed set is waived when it is covered by the owning Kustomization's `.spec.ignore` (Flux v2.9 DriftIgnoreRules) — Flux excludes those paths from drift detection, so blocking a manual edit would be incoherent (`allowed_drift_ignored_field`). Each ignore rule's `paths` are RFC 6901 JSON pointers; its optional `target` Selector (group/version/kind/name/namespace as anchored regexes + `labelSelector`/`annotationSelector`) is matched against the **old** object, mirroring `fluxcd/pkg/ssa/jsondiff`. The owner (and its `.spec.ignore`) is read from the controller-runtime cache **only on the would-deny path** (no extra RBAC; same get/list/watch on kustomizations). Fail-closed: an unreadable owner or a malformed matching rule waives nothing. **Limitation:** the value diff is schema-blind and collapses a keyed-list/array edit to the list path, so ignore pointers that descend into an array index or keyed-list member (e.g. `/spec/template/spec/containers/0/image`) do **not** waive — only pointers at or above the collapsed list path do. helm-controller has no `.spec.ignore`, so HelmRelease-owned objects are unaffected.
4. **Deletion in progress**: the object itself has a `deletionTimestamp` set (`allowed_deletion_in_progress`), **or** (DELETE only) its parent **namespace is terminating** (`allowed_namespace_terminating`). The namespace teardown case covers cascade deletes issued by the kube `namespace-controller`: finalizer-free Flux children are deleted directly and never carry their own `deletionTimestamp`, so without this the webhook would block teardown and wedge the namespace in `Terminating`. The signal is the parent namespace's `deletionTimestamp` (looked up via the controller-runtime cache, reusing `--namespace-fetch-timeout`; fail-closed if the namespace cannot be read), which a single manual child DELETE cannot fabricate, so live-namespace protection is unchanged. The cached lookup requires get/list/watch RBAC on namespaces (the informer performs a cluster-wide list+watch — `get` alone breaks the cache sync and silently disables both this bypass and the `--namespace-label` filter).
5. **No Flux-managed fields** (UPDATE and DELETE): if no Flux controller appears in `managedFields`, the resource was not applied by Flux — it carries Flux labels only by inheritance (e.g. an `Endpoints`/`EndpointSlice` that inherited its parent `Service`'s labels). The operation is allowed (`allowed_no_flux_managed_fields`).

For **CREATE**, `managedFields` are not yet populated. The owning Kustomization/HelmRelease `.status.inventory` is consulted **first** and is authoritative when readable:

6. **Owner inventory** (CREATE): the object's id (`<namespace>_<name>_<group>_<kind>`) is looked up in the owner `.status.inventory` (the owner is the one already fetched for multi-tenant SA resolution — no extra RBAC). If the id is **present**, the object is genuinely Flux-declared and only Flux may create it: **denied regardless of any `ownerReference` or requester identity** (`denied_create_flux_labels`) — the API server does not validate `ownerReferences` on CREATE, so a forged controller reference must not enable squatting. If the id is **absent**, Flux does not manage it; the labels are inherited from a parent (e.g. an operator-generated `VMServiceScrape` derived from a Flux-applied `ServiceMonitor`) — allowed (`allowed_not_in_owner_inventory`, after the heuristics below get a chance to attribute a more specific reason). Genuine Flux applies never reach this path — they arrive as the owning Flux controller and are allowed at signal 2.
7. **Controller `ownerReference`** (CREATE): if the object has a controller `ownerReference` (and its id is not vetoed by the inventory), it is a derived/owned child whose Flux labels are inherited from its parent (e.g. `EndpointSlice`→`Service`, `CertificateRequest`→`Certificate`, including operators outside `kube-system` such as cert-manager). Allowed (`allowed_owned_resource`).
8. **Recognised system controller** (CREATE and DELETE): requests from a recognised Kubernetes control-plane controller (defaults `kube-system:generic-garbage-collector`, `kube-system:endpoint-controller`, `kube-system:endpointslice-controller`, `kube-system:endpointslicemirroring-controller`, `kube-system:ttl-after-finished-controller`, `kube-system:cronjob-controller`, plus the non-SA component identities `system:apiserver` — the CRD finalizer deleting instances of a deleted CRD — and `system:kube-controller-manager` — KCM running without `--use-service-account-credentials`) are allowed (`allowed_system_controller`). On **CREATE** this covers classic `Endpoints`, which carry **no** `ownerReference`. On **DELETE** it covers legitimate lifecycle removal of Flux-applied resources — the garbage collector during cascade deletion, the TTL-after-finished/CronJob controllers cleaning up completed Jobs, and the apiserver's CRD cleanup (humans and tenants never carry these identities; full-username entries only match reserved non-SA `system:` usernames). The list is extensible via `--system-controller-sas` / `SYSTEM_CONTROLLER_SAS` (CSV of `namespace:name` SA shorthands or full `system:` usernames, merged with the defaults).

When the inventory **cannot be read** (owner missing, empty inventory, cache lag) and neither heuristic matches, the CREATE fails closed with the distinct reason `denied_create_inventory_unavailable`, so enforce-mode rollouts can tell owner/cache trouble from genuine squats.

> **Design note:** treating the inherited Flux **label** as proof of management is the root cause of false positives on derived objects. The non-inheritable proof of management is the Flux SSA `fieldManager` (mirrors Google Config Sync, which keys management off a self-referential `resource-id` annotation rather than an inheritable label).

### Dynamic GVK Discovery

The controller periodically queries `ServerGroupsAndResources()` and updates the `ValidatingWebhookConfiguration` with rules for each discovered GroupVersion. The `admissionregistration.k8s.io` group is excluded to prevent infinite loops.

Rules carry **`Scope: "*"`** — cluster-scoped Flux-managed objects (Namespaces, CRDs, ClusterRoles, PVs, …) are protected too; with `Namespaced` scope, `kubectl delete namespace` was an unguarded mass-delete primitive. The objectSelector keeps unlabelled cluster-scoped objects out of the webhook. Semantics worth knowing: for a **Namespace** object the request namespace is the namespace's own name (so the namespaced code paths apply, and the VWC `namespaceSelector` matches the namespace's own labels — system namespaces stay exempt); for **other** cluster-scoped kinds the request namespace is empty (always in scope; the `--namespace-label` filter and the namespace-terminating bypass do not apply). The legitimate way to delete a Flux-managed namespace is removing it from Git — Flux prunes it as the owning reconciler.

The VWC carries **two webhook entries** (`kustomize.<name>` and `helm.<name>`), each with an `objectSelector` requiring the corresponding Flux ownership label (`kustomize.toolkit.fluxcd.io/name` / `helm.toolkit.fluxcd.io/name`, operator `Exists`). Two entries express the OR a single label selector cannot: the API server only forwards requests for objects carrying at least one Flux ownership label, sparing every unrelated write a webhook round-trip. The selector matches the **old or the new** object on UPDATE/DELETE, so stripping the labels in a request cannot dodge interception; the in-handler `allowed_not_managed` gate remains as defence in depth (expect its metric volume to collapse once this pre-filter is live). An object carrying both labels is evaluated by both entries (idempotent decision, double-counted metrics — rare edge).

### Leader Election

Leader election is **opt-in** (default off) and driven by the `fluxcd/pkg/runtime` `leaderelection.Options` via `--enable-leader-election` (with tunable lease/renew/retry durations and release-on-cancel). The `LeaderElectionID` is `flux-drift-webhook-leader`. The deploy overlays pass `--enable-leader-election=true`. All replicas serve admission requests, but only the leader performs GVK discovery and updates the `ValidatingWebhookConfiguration`. This prevents conflicting SSA patches from multiple replicas.

### Manager Bootstrap

`cmd/webhook/main.go` bootstraps the controller-runtime manager from `fluxcd/pkg/runtime` v0.110.1 option structs, using `spf13/pflag` (not the stdlib `flag` package):

- **Logging**: `logger.NewLogger` / `logger.SetLogger` (the bespoke zap logger was removed in favour of `fluxcd/pkg/runtime/logger`)
- **Leader election**: `leaderelection.Options` (see above)
- **pprof**: `pprof.GetHandlers()` registered as extra handlers on the metrics server port
- **Probes**: `probes.SetupChecks`, plus a `readyz` check gated on the webhook server's `StartedChecker()` so the pod only reports Ready once the TLS listener is serving
- **Events**: a `fluxcd/pkg/runtime/events` recorder wired into the VWC controller (local Kubernetes Events only — no external notification-controller webhook)

> **Design note:** the `fluxcd/pkg/runtime/client` package is intentionally **not** imported to keep the binary lean — its impersonator pulls in `kubectl`/`kustomize`. The `--kube-api-qps`/`--kube-api-burst` flags are applied directly to the rest config from `ctrl.GetConfigOrDie` instead.

## Key Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--webhook-port` | `9443` | Webhook server port |
| `--metrics-bind-addr` | `:8080` | Metrics endpoint address |
| `--health-probe-bind-addr` | `:8081` | Health probe address |
| `--cert-dir` | `/certs` | TLS certificates directory |
| `--audit-only` | `false` | Log denials without blocking |
| `--log-level` | `info` | Log verbosity level (`trace`, `debug`, `info`, `error`) |
| `--log-encoding` | `json` | Log encoding format (`json` or `console`) |
| `--flux-namespace` | `flux-system` | Namespace where Flux is installed (env: `FLUX_NAMESPACE`) |
| `--webhook-name` | `flux-drift-webhook.fluxcd.io` | ValidatingWebhookConfiguration name (env: `WEBHOOK_NAME`) |
| `--discovery-interval` | `5m` | GVK discovery refresh interval (env: `DISCOVERY_INTERVAL`) |
| `--namespace-label` | *(empty)* | Optional: namespace label key to filter webhook scope |
| `--namespace-label-value` | *(empty)* | Optional: required label value (needs `--namespace-label`) |
| `--namespace-fetch-timeout` | `2s` | Timeout for namespace label lookups |
| `--system-controller-sas` | *(empty)* | Extra control-plane identities (CSV of `namespace:name` SA shorthands or full `system:` usernames) allowed to CREATE Flux-labelled derived resources and DELETE Flux-applied resources (lifecycle); merged with built-in defaults (env: `SYSTEM_CONTROLLER_SAS`) |
| `--enable-leader-election` | `false` | Enable leader election (only one active manager performs discovery/VWC updates) |
| `--leader-election-lease-duration` | `35s` | Interval non-leader candidates wait before force-acquiring leadership |
| `--leader-election-renew-deadline` | `30s` | Duration the leader retries refreshing leadership before giving up |
| `--leader-election-retry-period` | `5s` | Duration `LeaderElector` clients wait between action retries |
| `--leader-election-release-on-cancel` | `true` | Leader steps down voluntarily on manager shutdown |
| `--kube-api-qps` | `50.0` | Maximum queries-per-second of requests sent to the Kubernetes API |
| `--kube-api-burst` | `300` | Maximum burst queries-per-second of requests sent to the Kubernetes API |

The leader-election, logging (`--log-level`/`--log-encoding`) and `--kube-api-*` flags come from the `fluxcd/pkg/runtime` option structs (`leaderelection.Options`, `logger.Options`). The `--kube-api-qps`/`--kube-api-burst` values are applied to the rest config passed to `ctrl.GetConfigOrDie`.

Flags marked with `env:` can also be set via environment variables. CLI flags take precedence.

## Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `flux_drift_webhook_requests_total` | Counter | `operation`, `decision` | All admission requests processed |
| `flux_drift_webhook_denials_total` | Counter | `operation`, `kind` | Denied requests only |
| `flux_drift_webhook_ownership_conflicts_total` | Counter | `kind`, `previous_owner`, `new_owner` | Dual/multiple ownership: Flux owner labels flipped between two reconcilers (owners as `namespace/name`; recorded on each `denied_wrong_flux_controller`) |
| `flux_drift_webhook_latency_seconds` | Histogram | `operation` | Request processing latency |
| `flux_drift_webhook_discovery_errors_total` | Counter | — | GVK discovery errors |
| `flux_drift_webhook_config_updates_total` | Counter | `status` | VWC update attempts |

**Decision label values** for `requests_total`:
- `allowed_not_managed` — resource has no Flux labels
- `allowed_namespace_filter` — namespace excluded by label filter
- `allowed_owning_flux_controller` — request from the owning Flux controller
- `allowed_bypass_annotation` — bypass annotation present on existing object (UPDATE/DELETE only; never honoured on CREATE)
- `allowed_reconcile_disabled` — `kustomize.toolkit.fluxcd.io/reconcile: disabled` present on the existing object (Kustomization-owned; Flux skips it, so drift prevention is waived)
- `allowed_deletion_in_progress` — resource already being deleted (object has its own `deletionTimestamp`)
- `allowed_namespace_terminating` — DELETE while the parent namespace is terminating (cascade teardown by the namespace-controller)
- `allowed_no_flux_managed_fields` — no Flux SSA managed fields found (UPDATE, and DELETE of an inherited-label resource not applied by Flux)
- `allowed_no_field_conflict` — modified fields do not overlap with Flux-managed fields
- `allowed_drift_ignored_field` — UPDATE whose conflicting fields are all covered by the owning Kustomization's `.spec.ignore` (Flux DriftIgnoreRules); Flux excludes them from drift detection, so the edit is allowed
- `allowed_owned_resource` — CREATE of an object with a controller `ownerReference` (Flux labels inherited from its parent)
- `allowed_system_controller` — CREATE by a recognised control-plane controller (e.g. endpoint/endpointslice controllers), or DELETE of a Flux-applied resource by one (GC cascade, Job TTL/CronJob cleanup)
- `allowed_not_in_owner_inventory` — CREATE of an object whose id is absent from the owning Kustomization/HelmRelease `.status.inventory` (Flux labels inherited from a parent, e.g. an operator-derived resource)
- `allowed_subresource` — request for a sub-resource (status/scale/…); not subject to drift prevention
- `denied_parse_error` — failed to parse objects for field check (fail-closed)
- `denied_managed_fields_error` — failed to extract managed fields (fail-closed)
- `denied_managed_fields_tampered` — UPDATE releasing Flux field ownership without a value change (managedFields wipe / SSA manager-name spoof)
- `denied_bypass_annotation_added` — non-Flux UPDATE introducing a protection-disabling annotation (drift-prevention bypass or `reconcile: disabled`) on a Flux-applied object (two-step bypass attempt; apply it via Git instead)
- `denied_diff_error` — failed to compute field diff (fail-closed)
- `denied_create_flux_labels` — CREATE of an object whose id is declared in the owner inventory, by a non-Flux actor (squat protection; outranks ownerReference and system-controller identities)
- `denied_create_inventory_unavailable` — CREATE with Flux labels that no signal could clear: owner inventory unreadable/empty AND no controller ownerReference AND not a recognised system controller (fail-closed, distinct from squat for observability)
- `denied_delete_flux_managed` — DELETE of Flux-managed resource
- `denied_update_flux_managed_fields` — UPDATE modifying Flux-managed fields
- `denied_wrong_flux_controller` — Flux controller modifying another controller's resource
- `allowed_unknown_operation` — unknown operation type (defence-in-depth fallback)

## Prerequisites

- Go 1.26.0
- cert-manager installed in cluster
- FluxCD installed in `flux-system` namespace
- Kustomize 5.4+

### Key dependencies

| Dependency | Version |
|------------|---------|
| `sigs.k8s.io/controller-runtime` | v0.24.1 |
| `k8s.io/{api,apimachinery,client-go}` | v0.36.2 |
| `sigs.k8s.io/structured-merge-diff/v6` | v6.4.0 |
| `github.com/spf13/pflag` | v1.0.10 |
| `github.com/fluxcd/pkg/runtime` | v0.110.1 |

For the integration suite, `setup-envtest` provides the envtest assets (`ENVTEST_K8S_VERSION` `1.36.0`, fall back to `1.35.0` if `1.36.0` is not mirrored).

## Testing Strategy

### Unit tests

| File | Coverage |
|------|----------|
| `internal/webhook/handler_test.go` | All decision paths: not-managed, Flux controller (own + wrong), bypass annotation (valid + single-step attack + never on CREATE + introduction denied), `reconcile: disabled` (UPDATE/DELETE allow, Helm not honoured, introduction denied), deletion in progress, namespace-teardown cascade (terminating allow, active deny, fail-closed helper), CREATE (inventory veto incl. forged ownerReference and system-controller, not-in-inventory allow, empty/unreadable inventory fail-closed with distinct reason, RBAC colon ids, owned-resource/system-controller heuristics), DELETE deny (genuinely Flux-applied) + cluster-scoped (ClusterRole, Namespace), UPDATE protection with realistic apiserver-shaped fieldsV1 (container-image drift denied, HPA replicas allowed, label-strip denied, managedFields wipe denied), audit-only mode, fail-closed error paths, HelmRelease label paths, sub-resource allow, non-SA `system:apiserver` DELETE allow, UPDATE `.spec.ignore` waiver (exact-path/label-selector/list-path waived; target mismatch/array-index-path/owner-unreadable/Helm-owner denied; ignore cannot disarm ownership of other fields) |
| `internal/webhook/fields_test.go` | SSA managedFields extraction, value-based field diff, hierarchy-aware conflict detection (keyed-list ancestor/descendant overlap, disjoint and sibling non-conflicts), concurrent kustomize + helm managers (union of fields), `WaiveIgnoredConflicts` set algebra (exact/descendant waived, ancestor not waived, nil operands) |
| `internal/webhook/ignore_test.go` | `.spec.ignore` parsing (`parseIgnoreRules`), RFC 6901 JSON-pointer conversion (`jsonPointerToPath` escaping/root/error), full target-Selector matching (`ruleMatchesObject` anchored gvk/name/ns regexes, label/annotation selectors, invalid-input errors), `ignoreSetForObject` (target filtering, fail-closed) |
| `internal/webhook/auth_test.go` | Service account identity parsing (`IsFluxController` 11 edge cases; `IsSystemController` control-plane allow-list incl. configurable entry, non-SA full usernames `system:apiserver`/`system:kube-controller-manager`, and the non-`system:` spoof guard) |
| `internal/webhook/labels_test.go` | Flux label detection, bypass annotation check |
| `internal/controller/webhook_config_test.go` | VWC reconciliation, rule building, empty version skipping |
| `internal/discovery/discovery_test.go` | GVK discovery, excluded groups filtering |
| `internal/metrics/metrics_test.go` | Metrics registration, counter recording |
| `cmd/webhook/main_test.go` | `getEnv` (`TestGetEnv`), `getEnvDuration` (`TestGetEnvDuration`), `mergeSystemControllerSAs` (`TestMergeSystemControllerSAs`) helpers — the bespoke `setupLogger` helper was removed (logging is now `fluxcd/pkg/runtime/logger`) |

### Integration tests (envtest)

A `//go:build integration` suite lives in `internal/controller/{suite_test.go, webhook_config_integration_test.go}` and runs against a real apiserver provided by envtest (`make test-integration`). It uses Gomega (`NewWithT`) and drives `Reconcile` directly with a cacheless client. It asserts:

- real SSA `managedFields` (fieldManager `flux-drift-webhook`)
- live discovery rules built from the apiserver
- the `admissionregistration.k8s.io` group is excluded
- apiserver defaulting of the VWC
- the `ConfigUpdated` Event is emitted
- `ForceOwnership` idempotency (repeated Applies are stable)

> **Note:** `setup-envtest` fetches the kube-apiserver/etcd binary assets from an external GitHub index (not GOPROXY); GitHub-hosted runners have the network access to fetch them, so the CI job is a **required automatic gate**. Fallbacks in restricted networks: pre-seed the assets (`ENVTEST_INSTALLED_ONLY=1`) or point `--index` at a mirror.

### Fuzzing

Native Go fuzz targets live in `internal/webhook/fuzz_test.go`: `Fuzz_extractMetadata`, `Fuzz_ComputeFieldDiff`, `Fuzz_FluxManagedFields`, and `Fuzz_jsonPointerToPath` (the RFC 6901 parser for `.spec.ignore` paths). The seed corpora run as part of `make test`; the targets are exercised under `make fuzz-smoketest` (each runs for `FUZZ_TIME`, default 20s).

### Integration tests (`e2e/test-webhook.sh`)

Run against a live cluster with the webhook in audit-only mode (`make test-webhook`). Exercises the decision paths:

| Test | Decision Path | Assertion |
|------|--------------|-----------|
| T1 | `allowed_not_managed` (CREATE) | No audit warning |
| T2 | `allowed_not_managed` (UPDATE) | No audit warning |
| T3 | `allowed_not_managed` (DELETE) | No audit warning |
| T4 | `denied_create_inventory_unavailable` (owner absent on the e2e cluster) | Audit warning present |
| T5 | `allowed_no_flux_managed_fields` (UPDATE) | No audit warning |
| T6 | `denied_delete_flux_managed` (genuinely Flux-applied via SSA `kustomize-controller`) | Audit warning present |
| T6b | `allowed_no_flux_managed_fields` (DELETE of inherited-label resource) | No audit warning |
| T7 | `allowed_bypass_annotation` | No audit warning |
| T8 | Excluded namespace (kube-system) | No audit warning (VWC excluded) |
| T9 | `allowed_no_flux_managed_fields` (UPDATE data only) | No audit warning |
| T10 | `allowed_namespace_terminating` (cascade deletes during ns teardown) | No would-deny in webhook logs |
| T11 | `denied_delete_flux_managed` (DELETE of a Flux-applied Namespace, Scope `*`) | Audit warning present |

### E2E tests (`e2e/run-e2e.sh`)

Full end-to-end: creates a kind cluster, installs cert-manager, deploys the webhook, then runs the integration tests above (`make test-e2e`). Requires a local cert-manager manifest at `e2e/cert-manager.yaml`.

## CI/CD Pipeline

Two GitHub Actions workflows live in `.github/workflows/`.

**`ci.yaml`** runs on every pull request and push to `main`:

| Job | Purpose |
|-----|---------|
| `lint` | `golangci-lint` with `.golangci.yml` (gosec runs as a configured linter) |
| `test` | `go vet`, `gofmt` check, unit tests with `-race` and coverage |
| `verify-codegen` | fails if `go mod tidy` / `go generate` leave the tree dirty |
| `fuzz` | `make fuzz-smoketest` (native Go fuzz targets) |
| `integration` | `make test-integration` (envtest against a real apiserver) |
| `manifests` | `kubeconform` validation of `deploy/base` and both overlays |
| `helm` | `helm lint` plus rendered-template `kubeconform` validation |
| `build` | multi-arch container image build (`linux/amd64,linux/arm64`), no push |

**`release.yaml`** runs on `v*` tags: it builds and pushes a multi-arch image to
`ghcr.io/pmialon/flux-drift-webhook` (with SBOM and build provenance), signs it with cosign
(keyless, GitHub OIDC), generates SLSA provenance, publishes the Helm chart as an OCI artefact to
`oci://ghcr.io/pmialon/charts`, and creates a GitHub release via GoReleaser (source archive, SBOM,
checksums, release notes).

Jobs are wired to the `make` targets above, so the local `make verify` / `make ci` gate mirrors CI.

## Troubleshooting

**Webhook is blocking legitimate requests:**
1. Check the denial reason in `kubectl` output (returned as admission response message)
2. Check webhook logs: `kubectl logs -n flux-system deploy/flux-drift-webhook -f`
3. Look for the `decision` label in `flux_drift_webhook_requests_total` metric
4. To identify the owning Kustomization/HelmRelease, check the resource's labels

**Emergency bypass:**
1. Add the bypass annotation via Git: `fluxcd.io/drift-prevention-bypass: disabled`
2. Wait for Flux to reconcile — the annotation will be applied to the existing object
3. Subsequent manual changes will be allowed until the annotation is removed

**Abandoning a resource (un-manage without deleting):**
1. Add `kustomize.toolkit.fluxcd.io/reconcile: disabled` (and `kustomize.toolkit.fluxcd.io/prune: disabled` if you intend to remove it from Git) via Git
2. Wait for Flux to reconcile — kustomize-controller now skips the object, and the webhook waives drift prevention (`allowed_reconcile_disabled`)
3. Optionally remove the manifest from Git: prune is skipped, the object stays and is freely manageable
4. Kustomization-owned objects only — helm-controller has no per-object equivalent

**HPA/VPA/KEDA updates blocked:**
- This means Flux manages `.spec.replicas` via SSA (it appears in managedFields)
- Fix: remove `.spec.replicas` from the Flux-managed manifest so SSA no longer claims that field
- After Flux reconciles, the HPA can update `.spec.replicas` freely

**`denied_wrong_flux_controller` errors:**
- Two Kustomisations or HelmReleases are managing the same resource with different labels
- Fix: ensure each resource is owned by exactly one Kustomisation/HelmRelease

**Certificate/TLS errors:**
- The webhook requires cert-manager; verify the Certificate and Issuer resources are healthy
- Check: `kubectl get certificate -n flux-system flux-drift-webhook`
- Check: `kubectl get issuer -n flux-system flux-drift-webhook-issuer`

**Webhook unavailable:**
- The VWC uses `failurePolicy: Ignore` — if the webhook is down, all operations are allowed
- A PodDisruptionBudget (`minAvailable: 1`) prevents all pods being evicted simultaneously
- Monitor `flux_drift_webhook_requests_total` for gaps to detect outage windows

## Reference Documentation

- [Google Config Sync](https://cloud.google.com/anthos-config-management/docs/how-to/preventing-local-modifications) — the drift-prevention feature that inspired this webhook
- [FluxCD documentation](https://fluxcd.io/flux/) — GitOps toolkit for Kubernetes
