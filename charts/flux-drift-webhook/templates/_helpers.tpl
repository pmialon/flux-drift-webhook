{{/*
Expand the name of the chart.
*/}}
{{- define "flux-drift-webhook.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. For the recommended install (release name
"flux-drift-webhook" in namespace flux-system) this resolves to
"flux-drift-webhook", matching deploy/base 1:1.
*/}}
{{- define "flux-drift-webhook.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "flux-drift-webhook.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels — NAME ONLY, matching deploy/base (kustomize includeSelectors:
false). These land in immutable selectors (Deployment, Service, PDB,
NetworkPolicy, PodMonitor, podAntiAffinity) and must stay name-only — do NOT add
app.kubernetes.io/instance here.
*/}}
{{- define "flux-drift-webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ include "flux-drift-webhook.name" . }}
{{- end -}}

{{/*
Common metadata labels (superset of the selector; safe on non-selector metadata).
*/}}
{{- define "flux-drift-webhook.labels" -}}
helm.sh/chart: {{ include "flux-drift-webhook.chart" . }}
{{ include "flux-drift-webhook.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/component: webhook
app.kubernetes.io/part-of: flux
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "flux-drift-webhook.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "flux-drift-webhook.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Service name. PINNED to the controller's hard-coded WebhookService
("flux-drift-webhook"); defaults to the chart name so it is stable across
release names. The webhook Service, the certificate DNS names, and the VWC
clientConfig must all use this.
*/}}
{{- define "flux-drift-webhook.serviceName" -}}
{{- default (include "flux-drift-webhook.name" .) .Values.service.name -}}
{{- end -}}

{{/*
ValidatingWebhookConfiguration name. Defaults to the controller's --webhook-name
default; the controller reconciles the VWC by this exact name.
*/}}
{{- define "flux-drift-webhook.webhookName" -}}
{{- default "flux-drift-webhook.fluxcd.io" .Values.config.webhookName -}}
{{- end -}}

{{- define "flux-drift-webhook.certName" -}}
{{- include "flux-drift-webhook.fullname" . -}}
{{- end -}}

{{- define "flux-drift-webhook.issuerName" -}}
{{- default (printf "%s-issuer" (include "flux-drift-webhook.fullname" .)) .Values.certManager.issuer.name -}}
{{- end -}}

{{/*
TLS secret mounted at config.certDir. With cert-manager it is the Certificate's
secretName; otherwise it is the externally-managed tls.secretName.
*/}}
{{- define "flux-drift-webhook.tlsSecretName" -}}
{{- if .Values.certManager.enabled -}}
{{- default (printf "%s-tls" (include "flux-drift-webhook.fullname" .)) .Values.certManager.certificate.secretName -}}
{{- else -}}
{{- required "tls.secretName is required when certManager.enabled=false" .Values.tls.secretName -}}
{{- end -}}
{{- end -}}

{{/*
Container image reference (digest wins over tag; tag defaults to appVersion).
*/}}
{{- define "flux-drift-webhook.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}
