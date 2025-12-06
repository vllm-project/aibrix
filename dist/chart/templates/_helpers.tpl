{{- define "aibrix.hasMutatingWebhooks" -}}
{{- $d := dict "found" false -}}
{{- range . -}}
  {{- if eq .type "mutating" -}}
    {{- $_ := set $d "found" true -}}
  {{- end -}}
{{- end -}}
{{- $d.found -}}
{{- end -}}

{{- define "aibrix.hasValidatingWebhooks" -}}
{{- $d := dict "found" false -}}
{{- range . -}}
  {{- if eq .type "validating" -}}
    {{- $_ := set $d "found" true -}}
  {{- end -}}
{{- end -}}
{{- $d.found -}}
{{- end }}

{{/*
Renders imagePullSecrets block
*/}}
{{- define "aibrix.imagePullSecrets" -}}
{{- $secrets := .componentSecrets | default .globalSecrets -}}
{{- if $secrets -}}
imagePullSecrets:
{{- toYaml $secrets | nindent 2 }}
{{- end -}}
{{- end -}}

{{/*
Expand the name of the chart.
*/}}
{{- define "aibrix.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "aibrix.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "aibrix.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "aibrix.labels" -}}
helm.sh/chart: {{ include "aibrix.chart" . }}
{{ include "aibrix.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "aibrix.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aibrix.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "aibrix.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "aibrix.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
