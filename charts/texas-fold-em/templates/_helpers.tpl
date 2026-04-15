{{/*
Fully-qualified resource name. Follows the Helm canonical pattern:
  - fullnameOverride wins outright.
  - If the release name already contains the chart name, use it as-is
    (avoids "foo-foo" for releases named after the chart).
  - Otherwise, prefix release name with chart name.
Truncated to 63 chars for DNS-1123 compliance.
*/}}
{{- define "texas-fold-em.fullname" -}}
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

{{/*
Labels applied to every object. Merges in commonLabels for the caller.
*/}}
{{- define "texas-fold-em.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels — the stable subset used for Service.selector and
StatefulSet.spec.selector. These must NEVER change across upgrades.
*/}}
{{- define "texas-fold-em.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Name of the Secret holding the two bearer keys.
*/}}
{{- define "texas-fold-em.secretName" -}}
{{- if .Values.secret.existingSecret -}}
{{ .Values.secret.existingSecret }}
{{- else -}}
{{ include "texas-fold-em.fullname" . }}
{{- end -}}
{{- end -}}
