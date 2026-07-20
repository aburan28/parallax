{{/*
Common template helpers for the parallax chart.
*/}}

{{- define "parallax.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name.
*/}}
{{- define "parallax.fullname" -}}
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

{{- define "parallax.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels (metadata only). Component is NOT included here — each workload
(operator, metrics service, localpostgres) stamps its own component to avoid
duplicate label keys.
*/}}
{{- define "parallax.labels" -}}
helm.sh/chart: {{ include "parallax.chart" . }}
{{ include "parallax.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: parallax
{{- end -}}

{{/*
Base identity labels shared by all parallax objects (no component — add one
explicitly per workload so selectors stay unambiguous and keys never duplicate).
*/}}
{{- define "parallax.selectorLabels" -}}
app.kubernetes.io/name: {{ include "parallax.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Operator pod selector labels (base identity + the operator component). Used by
the Deployment selector/template, the metrics Service selector, and the NetworkPolicy.
*/}}
{{- define "parallax.operatorSelectorLabels" -}}
{{ include "parallax.selectorLabels" . }}
app.kubernetes.io/component: operator
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "parallax.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "parallax.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Fully-qualified operator image reference (tag defaults to appVersion).
*/}}
{{- define "parallax.image" -}}
{{- $tag := default .Chart.AppVersion .Values.operator.tag -}}
{{- printf "%s:%s" .Values.operator.image $tag -}}
{{- end -}}

{{/*
Fully-qualified plugin image reference (tag defaults to appVersion).
*/}}
{{- define "parallax.pluginImage" -}}
{{- $tag := default .Chart.AppVersion .Values.pluginImage.tag -}}
{{- printf "%s:%s" .Values.pluginImage.repository $tag -}}
{{- end -}}

{{/*
Name of the Secret holding the local-postgres credentials.
*/}}
{{- define "parallax.localPostgres.secretName" -}}
{{- printf "%s-postgres" (include "parallax.fullname" .) -}}
{{- end -}}

{{/*
Name of the local-postgres Service.
*/}}
{{- define "parallax.localPostgres.serviceName" -}}
{{- printf "%s-postgres" (include "parallax.fullname" .) -}}
{{- end -}}
