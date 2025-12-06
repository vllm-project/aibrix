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

{{/*
Create the name of the controller manager service account
*/}}
{{- define "aibrix.controllerManager.serviceAccountName" -}}
{{- if and .Values.controllerManager.serviceAccount.create (eq .Values.controllerManager.serviceAccount.name "") -}}
{{- $sa := default (include "aibrix.fullname" .) .Values.controllerManager.serviceAccount.name }}
{{- printf "%s-controller-manager" $sa }}
{{- else if and .Values.controllerManager.serviceAccount.create (not (eq .Values.controllerManager.serviceAccount.name "")) -}}
{{- .Values.controllerManager.serviceAccount.name -}}
{{- else -}}
{{- include "aibrix.serviceAccountName" . -}}
{{- end -}}
{{- end -}}

{{/*
Create the name of the gateway plugin service account
*/}}
{{- define "aibrix.gatewayPlugin.serviceAccountName" -}}
{{- if and .Values.gatewayPlugin.serviceAccount.create (eq .Values.gatewayPlugin.serviceAccount.name "") -}}
{{- $sa := default (include "aibrix.fullname" .) .Values.gatewayPlugin.serviceAccount.name }}
{{- printf "%s-gateway-plugin" $sa }}
{{- else if and .Values.gatewayPlugin.serviceAccount.create (not (eq .Values.gatewayPlugin.serviceAccount.name "")) -}}
{{- .Values.gatewayPlugin.serviceAccount.name -}}
{{- else -}}
{{- include "aibrix.serviceAccountName" . -}}
{{- end -}}
{{- end -}}


{{/*
Create the name of the gateway plugin service account
*/}}
{{- define "aibrix.gpuOptimizer.serviceAccountName" -}}
{{- if and .Values.gpuOptimizer.serviceAccount.create (eq .Values.gpuOptimizer.serviceAccount.name "") -}}
{{- $sa := default (include "aibrix.fullname" .) .Values.gpuOptimizer.serviceAccount.name }}
{{- printf "%s-gpu-optimizer" $sa }}
{{- else if and .Values.gpuOptimizer.serviceAccount.create (not (eq .Values.gpuOptimizer.serviceAccount.name "")) -}}
{{- .Values.gpuOptimizer.serviceAccount.name -}}
{{- else -}}
{{- include "aibrix.serviceAccountName" . -}}
{{- end -}}
{{- end -}}

{{/*
Create the name of the metadata service service account
*/}}
{{- define "aibrix.metadata.serviceAccountName" -}}
{{- if and .Values.metadata.serviceAccount.create (eq .Values.metadata.serviceAccount.name "") -}}
{{- $sa := default (include "aibrix.fullname" .) .Values.metadata.serviceAccount.name }}
{{- printf "%s-metadata-service" $sa }}
{{- else if and .Values.metadata.serviceAccount.create (not (eq .Values.metadata.serviceAccount.name "")) -}}
{{- .Values.metadata.serviceAccount.name -}}
{{- else -}}
{{- include "aibrix.serviceAccountName" . -}}
{{- end -}}
{{- end -}}
