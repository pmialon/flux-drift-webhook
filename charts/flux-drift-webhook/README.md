# flux-drift-webhook Helm chart

A Helm chart to install **flux-drift-webhook** — a validating admission webhook that prevents manual
drift on FluxCD-managed resources.

This chart is an **alternative** to the Kustomize manifests under [`deploy/`](../../deploy); pick one.
Both render the same set of resources.

## Prerequisites

- Kubernetes >= 1.27
- [cert-manager](https://cert-manager.io) (default TLS path; or bring your own — see below)
- FluxCD installed in the target namespace (`flux-system`)
- Prometheus Operator CRDs, if `podMonitor.enabled=true` (default)

## Install

The chart is published as an OCI artefact. Install it into the Flux namespace with the recommended
release name `flux-drift-webhook`:

```sh
helm install flux-drift-webhook \
  oci://ghcr.io/pmialon/charts/flux-drift-webhook \
  --version <x.y.z> \
  --namespace flux-system --create-namespace
```

> The release **must** run in the Flux namespace (`flux-system` by default). The controller's webhook
> Service name is pinned to `flux-drift-webhook`; do not change `service.name`.

Audit-only (log without blocking):

```sh
helm install flux-drift-webhook oci://… -n flux-system --set config.auditOnly=true
```

## GitOps with Flux

Let Flux manage the release. Point a Flux OCI `HelmRepository` at the chart's registry and have it
check for new chart releases every ten minutes:

```sh
flux create source helm flux-drift-webhook \
  --namespace=flux-system \
  --url=oci://ghcr.io/pmialon/charts \
  --type=oci \
  --interval=10m
```

Create a `flux-drift-webhook-values.yaml` file locally (start in audit mode; wire the PodMonitor to
your Prometheus stack):

```sh
cat > flux-drift-webhook-values.yaml <<EOL
config:
  auditOnly: true
podMonitor:
  additionalLabels:
    release: kube-prometheus-stack
EOL
```

Create a `HelmRelease` to deploy the webhook in the `flux-system` namespace (the chart must run there
— the controller pins the webhook Service name):

```sh
flux create helmrelease flux-drift-webhook \
  --namespace=flux-system \
  --source=HelmRepository/flux-drift-webhook \
  --release-name=flux-drift-webhook \
  --chart=flux-drift-webhook \
  --chart-version=">=0.1.0" \
  --values=flux-drift-webhook-values.yaml
```

Flux upgrades the release automatically when a new chart version is published. If an upgrade fails,
Flux can [roll back](https://fluxcd.io/flux/components/helm/helmreleases/#configuring-failure-remediation)
to the previous working version.

Check the currently deployed version:

```sh
flux get helmreleases -n flux-system
```

To remove the source and release from your cluster:

```sh
flux -n flux-system delete source helm flux-drift-webhook
flux -n flux-system delete helmrelease flux-drift-webhook
```

To manage the source and release **declaratively in Git** (rather than via the CLI above), commit the
generated `HelmRepository`/`HelmRelease` manifests and let Flux reconcile them — see the Flux
[multi-env example](https://github.com/fluxcd/flux2-kustomize-helm-example) with Kustomize and Helm.

## Values

| Key | Description | Default |
|-----|-------------|---------|
| `image.repository` | Image repository | `ghcr.io/pmialon/flux-drift-webhook` |
| `image.tag` | Image tag (defaults to chart `appVersion`) | `""` |
| `image.digest` | Pin by digest (`sha256:…`); wins over `tag` | `""` |
| `replicaCount` | Replicas (ignored when `autoscaling.enabled`) | `3` |
| `config.auditOnly` | Audit-only mode (vs enforce) | `false` |
| `config.logEncoding` | `json` or `console` | `json` |
| `config.logLevel` | `trace`/`debug`/`info`/`error` (empty = binary default) | `""` |
| `config.leaderElection.enabled` | Leader election | `true` |
| `config.kubeApiQps` / `kubeApiBurst` | Client rate limits (empty = binary defaults 50/300) | `""` |
| `resources` | Requests/limits (Guaranteed QoS) | `2` CPU / `256Mi` |
| `runtime.gomaxprocs` / `gomemlimit` | Go runtime tuning (couple to `resources`) | `"2"` / `"230MiB"` |
| `autoscaling.enabled` | HPA (CPU-only) | `true` (min 3 / max 9 / 80%) |
| `podDisruptionBudget.enabled` | PDB `minAvailable: 1` | `true` |
| `networkPolicy.enabled` | Ingress-only NetworkPolicy on the webhook port | `true` |
| `podMonitor.enabled` | Prometheus Operator PodMonitor | `true` |
| `certManager.enabled` | Manage TLS via cert-manager (self-signed Issuer + Certificate) | `true` |
| `tls.secretName` / `tls.caBundle` | External TLS secret + CA, required when `certManager.enabled=false` | `""` |
| `service.name` | Webhook Service name — **pinned** to the controller's expectation | `flux-drift-webhook` |
| `serviceAccount.create` / `rbac.create` | Create SA / ClusterRole(+Binding) | `true` |
| `tests.enabled` | Render a `helm test` connectivity probe | `true` |

The ClusterRole rules and the ValidatingWebhookConfiguration name
(`flux-drift-webhook.fluxcd.io`) are fixed — the controller depends on them and populates the
webhook `rules` at runtime via server-side apply, so the chart ships an empty `rules: []` bootstrap.

## TLS without cert-manager

```sh
helm install flux-drift-webhook oci://… -n flux-system \
  --set certManager.enabled=false \
  --set tls.secretName=my-webhook-tls \
  --set tls.caBundle=<base64-CA>
```

## Uninstall

```sh
helm uninstall flux-drift-webhook -n flux-system
```
