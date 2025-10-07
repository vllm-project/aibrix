{{/*
Expand the name of the chart.
*/}}
{{- define "aibrix-model.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "aibrix-model.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "aibrix-model.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "aibrix-model.labels" -}}
helm.sh/chart: {{ include "aibrix-model.chart" . }}
{{ include "aibrix-model.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
engine: {{ .Values.engine.type | quote }}
deployment: {{ .Values.deployment.type | quote }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "aibrix-model.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aibrix-model.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Get engine image with tag
*/}}
{{- define "aibrix-model.engineImage" -}}
{{- if .Values.engine.image -}}
{{ .Values.engine.image }}
{{- else if eq .Values.engine.type "sglang" -}}
kvcache-container-image-hb2-cn-beijing.cr.volces.com/aibrix:sglang:v0.4.9.post3-cu126-nixl-v0.4.1
{{- else if eq .Values.engine.type "vllm" -}}
kvcache-container-image-hb2-cn-beijing.cr.volces.com/aibrix:vllm-openai:v0.9.2-cu128-nixl-v0.4.1-lmcache-0.3.1.post1
{{- end -}}
{{- end }}

{{/*
Get router image with tag  
*/}}
{{- define "aibrix-model.routerImage" -}}
{{ .Values.router.image }}
{{- end }}

{{/*
RDMA network annotation
*/}}
{{- define "aibrix-model.rdmaAnnotation" -}}
{{- if .Values.network.rdma.enabled -}}
k8s.volcengine.com/pod-networks: |
  [
    {
      "cniConf":{
          "name":"{{ .Values.network.rdma.cniName }}"
      }
    }
  ]
{{- end -}}
{{- end }}

{{/*
Environment variables for all containers
*/}}
{{- define "aibrix-model.envVars" -}}
{{- range .Values.env }}
{{- if eq $.Values.engine.type "vllm" }}
- name: {{ .name }}
  value: {{ .value | quote }}
{{- else if eq $.Values.engine.type "sglang" }}
{{- if not (hasPrefix "VLLM_" .name) }}
- name: {{ .name }}
  value: {{ .value | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Resource limits
*/}}
{{- define "aibrix-model.resources" -}}
limits:
  nvidia.com/gpu: {{ .Values.resources.gpu }}
{{- if .Values.resources.rdma }}
  vke.volcengine.com/rdma: {{ .Values.resources.rdma | quote }}
{{- end }}
{{- end }}

