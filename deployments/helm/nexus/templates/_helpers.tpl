{{/*
Name helpers. Standard Helm boilerplate, kept because every downstream template depends on
the 63-character truncation being applied consistently - a release name long enough to
overflow a label value produces errors far from where the mistake was made.
*/}}
{{- define "nexus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nexus.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "nexus.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels: the app.kubernetes.io/* recommended set, applied to every object the chart
creates. app.kubernetes.io/part-of is what makes `kubectl get all -l app.kubernetes.io/
part-of=nexus` return the whole system rather than one component at a time.
*/}}
{{- define "nexus.labels" -}}
helm.sh/chart: {{ include "nexus.chart" . }}
{{ include "nexus.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: nexus
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels: name + instance only.

These land in Deployment.spec.selector, which is IMMUTABLE after creation. Nothing volatile
belongs here - notably not app.kubernetes.io/version, because then every release would
require deleting and recreating every Deployment.
*/}}
{{- define "nexus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nexus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Per-component variants. Called as:
  {{- include "nexus.componentLabels" (dict "root" $ "component" "api") | nindent 4 }}
*/}}
{{- define "nexus.componentLabels" -}}
{{ include "nexus.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "nexus.componentSelectorLabels" -}}
{{ include "nexus.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "nexus.serviceAccountName" -}}
{{- if .root.Values.serviceAccount.create }}
{{- printf "%s-%s" (include "nexus.fullname" .root) .component }}
{{- else }}
{{- default "default" .root.Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference builder.

Two things happen here beyond string concatenation:

  1. An empty component tag falls back to .Chart.AppVersion, so a release bump is a single
     edit in Chart.yaml rather than four values.
  2. The tag `latest` is rejected outright. A mutable tag means two pods reporting the same
     "version" can be running different code, which makes incidents unreproducible and
     rollbacks meaningless. Failing the render is noisy on purpose - it is much cheaper to
     fix here than to discover at 3am that "rollback to the previous tag" did nothing.
*/}}
{{- define "nexus.image" -}}
{{- $registry := .root.Values.global.imageRegistry | default "" -}}
{{- $repo := .image.repository -}}
{{- $tag := .image.tag | default .root.Chart.AppVersion | toString -}}
{{- if eq $tag "latest" -}}
{{- fail (printf "image tag 'latest' is not allowed for %s - pin an immutable version" $repo) -}}
{{- end -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Name of the Secret holding NEXUS_DATABASE_URL, NEXUS_AUTH_JWT_SECRET and
NEXUS_WEBHOOK_SECRET. Either the one this chart renders, or a pre-existing one supplied by
External Secrets / Sealed Secrets / a bootstrap script.
*/}}
{{- define "nexus.secretName" -}}
{{- if .Values.secrets.create -}}
{{- printf "%s-secrets" (include "nexus.fullname" .) -}}
{{- else -}}
{{- required "secrets.existingSecret is required when secrets.create is false - NEXUS will not start without NEXUS_DATABASE_URL, NEXUS_AUTH_JWT_SECRET and NEXUS_WEBHOOK_SECRET" .Values.secrets.existingSecret -}}
{{- end -}}
{{- end }}

{{/*
NATS connection URL: the bundled in-cluster service, or the external endpoint. Resolved in
one place so no template has to re-derive it and get it subtly wrong.
*/}}
{{- define "nexus.natsUrl" -}}
{{- if .Values.nats.enabled -}}
{{- printf "nats://%s-nats.%s.svc.cluster.local:4222" (include "nexus.fullname" .) .Release.Namespace -}}
{{- else -}}
{{- required "nats.url is required when nats.enabled is false" .Values.nats.url -}}
{{- end -}}
{{- end }}

{{/*
Prometheus scrape annotations. Every NEXUS workload exposes /metrics on 9090.
*/}}
{{- define "nexus.metricsAnnotations" -}}
{{- if .Values.metrics.podAnnotations.enabled }}
prometheus.io/scrape: "true"
prometheus.io/port: "9090"
prometheus.io/path: "/metrics"
{{- end }}
{{- end }}

{{/*
Image pull secrets, hoisted so each pod spec is one include instead of four lines of
conditional YAML.
*/}}
{{- define "nexus.imagePullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/*
envFrom block shared by api, worker and reconciler: the non-secret ConfigMap plus the
credential Secret. Keeping it in one helper means adding a new configuration source is a
one-line change rather than a four-file change that someone forgets to finish.
*/}}
{{- define "nexus.envFrom" -}}
- configMapRef:
    name: {{ include "nexus.fullname" . }}-config
- secretRef:
    name: {{ include "nexus.secretName" . }}
{{- end }}

{{/*
topologySpreadConstraints need a labelSelector matching the component, which the values
cannot express on their own without repeating the selector in every entry. This injects it.
*/}}
{{- define "nexus.topologySpreadConstraints" -}}
{{- range .constraints }}
- maxSkew: {{ .maxSkew }}
  topologyKey: {{ .topologyKey }}
  whenUnsatisfiable: {{ .whenUnsatisfiable }}
  labelSelector:
    matchLabels:
      {{- include "nexus.componentSelectorLabels" (dict "root" $.root "component" $.component) | nindent 6 }}
{{- end }}
{{- end }}
