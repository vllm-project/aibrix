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
Create the name of the controller manager service account
*/}}
{{- define "aibrix.controllerManager.serviceAccountName" -}}
{{- if .Values.controllerManager.serviceAccount.create -}}
{{- .Values.controllerManager.serviceAccount.name | default (printf "%s-controller-manager" (include "aibrix.fullname" .)) -}}
{{- else -}}
{{- .Values.controllerManager.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/*
Create the name of the gateway plugin service account
*/}}
{{- define "aibrix.gatewayPlugin.serviceAccountName" -}}
{{- if .Values.gatewayPlugin.serviceAccount.create -}}
{{- .Values.gatewayPlugin.serviceAccount.name | default (printf "%s-gateway-plugin" (include "aibrix.fullname" .)) -}}
{{- else -}}
{{- .Values.gatewayPlugin.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}


{{/*
Create the name of the gpu optimizer service account
*/}}
{{- define "aibrix.gpuOptimizer.serviceAccountName" -}}
{{- if .Values.gpuOptimizer.serviceAccount.create -}}
{{- .Values.gpuOptimizer.serviceAccount.name | default (printf "%s-gpu-optimizer" (include "aibrix.fullname" .)) -}}
{{- else -}}
{{- .Values.gpuOptimizer.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/*
Create the name of the metadata service service account
*/}}
{{- define "aibrix.metadata.serviceAccountName" -}}
{{- if .Values.metadata.serviceAccount.create -}}
{{- .Values.metadata.serviceAccount.name | default (printf "%s-metadata-service" (include "aibrix.fullname" .)) -}}
{{- else -}}
{{- .Values.metadata.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{- define "aibrix.validateValues" -}}
{{- if and .Values.gateway.enable .Values.gateway.envoyAsSideCar -}}
{{- fail "gateway.enable and gateway.envoyAsSideCar are mutually exclusive and cannot both be true." -}}
{{- end -}}
{{- end -}}
