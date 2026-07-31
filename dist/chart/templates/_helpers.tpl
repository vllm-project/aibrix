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

{{- $builtInRedisEnabled := dig "redis" "enabled" true .Values.metadata -}}
{{- $sharedEnablePassword := dig "redis" "enablePassword" false .Values.metadata -}}
{{- $sharedPassword := dig "redis" "password" "" .Values.metadata -}}

{{- /* Shared Redis password must be non-empty when enablePassword is true */ -}}
{{- if and $sharedEnablePassword (empty (trim $sharedPassword)) -}}
{{- fail "metadata.redis.enablePassword=true requires a non-empty metadata.redis.password." -}}
{{- end -}}

{{- /* When builtIn Redis is disabled, all components must declare an external Redis host */ -}}
{{- if not $builtInRedisEnabled -}}
{{- $missingHosts := list -}}
{{- if empty (trim (dig "service" "redis" "host" "" .Values.metadata)) -}}
  {{- $missingHosts = append $missingHosts "metadata.service.redis.host" -}}
{{- end -}}
{{- if empty (trim (dig "dependencies" "redis" "host" "" .Values.gatewayPlugin)) -}}
  {{- $missingHosts = append $missingHosts "gatewayPlugin.dependencies.redis.host" -}}
{{- end -}}
{{- if empty (trim (dig "dependencies" "redis" "host" "" .Values.gpuOptimizer)) -}}
  {{- $missingHosts = append $missingHosts "gpuOptimizer.dependencies.redis.host" -}}
{{- end -}}

{{- if gt (len $missingHosts) 0 -}}
{{- fail (printf "metadata.redis.enabled=false requires non-empty values for %s." (join ", " $missingHosts)) -}}
{{- end -}}
{{- end -}}

{{- /*
  Prevent the use of component-specific Redis passwords when the shared builtIn Redis is enabled.
  This OR block checks whether any custom password is set across the three components.
*/ -}}
{{- if and $builtInRedisEnabled (or
  (dig "service" "redis" "password" "" .Values.metadata)
  (dig "dependencies" "redis" "password" "" .Values.gatewayPlugin)
  (dig "dependencies" "redis" "password" "" .Values.gpuOptimizer)) -}}
{{- fail "built-in Redis does not support component-specific passwords; use metadata.redis.enablePassword/password for shared built-in Redis auth or disable built-in Redis for external Redis passwords." -}}
{{- end -}}

{{- if and
  (dig "auth" "bearerToken" "" .Values.gatewayPlugin)
  (hasKey (dig "container" "envs" dict .Values.gatewayPlugin) "AIBRIX_AUTH_BEARER_TOKEN") -}}
{{- fail "gatewayPlugin.auth.bearerToken and gatewayPlugin.container.envs.AIBRIX_AUTH_BEARER_TOKEN are mutually exclusive; configure the gateway API key in only one place." -}}
{{- end -}}

{{- end -}}

{{/*
Return whether the shared Redis password config is enabled.
*/}}
{{- define "aibrix.redis.sharedHasPassword" -}}
{{- if or (dig "redis" "enablePassword" false .Values.metadata) (dig "redis" "password" "" .Values.metadata) -}}true{{- end -}}
{{- end -}}

{{/*
Return whether a component Redis connection should use password auth.
Supported components: metadataService, gatewayPlugin, gpuOptimizer.
*/}}
{{- define "aibrix.redis.connectionHasPassword" -}}
{{- $sharedEnabled := dig "redis" "enablePassword" false .Values.metadata -}}
{{- $sharedPassword := dig "redis" "password" "" .Values.metadata -}}

{{- $componentPassword := "" -}}
{{- if eq .component "metadataService" -}}
  {{- $componentPassword = dig "service" "redis" "password" "" .Values.metadata -}}
{{- else if eq .component "gatewayPlugin" -}}
  {{- $componentPassword = dig "dependencies" "redis" "password" "" .Values.gatewayPlugin -}}
{{- else if eq .component "gpuOptimizer" -}}
  {{- $componentPassword = dig "dependencies" "redis" "password" "" .Values.gpuOptimizer -}}
{{- end -}}

{{- if or $sharedEnabled $sharedPassword $componentPassword -}}true{{- end -}}
{{- end -}}

{{/*
Return whether the Redis Secret should be rendered.
*/}}
{{- define "aibrix.redis.anyPasswordsConfigured" -}}
{{- if or
  (include "aibrix.redis.sharedHasPassword" .)
  (include "aibrix.redis.connectionHasPassword" (dict "Values" .Values "component" "metadataService"))
  (include "aibrix.redis.connectionHasPassword" (dict "Values" .Values "component" "gatewayPlugin"))
  (include "aibrix.redis.connectionHasPassword" (dict "Values" .Values "component" "gpuOptimizer")) -}}true{{- end -}}
{{- end -}}

{{/*
Return the Redis password Secret key name for a component.
*/}}
{{- define "aibrix.redis.connectionPasswordKey" -}}
{{- if eq .component "metadataService" -}}metadata-service-redis-password
{{- else if eq .component "gatewayPlugin" -}}gateway-plugin-redis-password
{{- else if eq .component "gpuOptimizer" -}}gpu-optimizer-redis-password
{{- else if eq .component "redis" -}}redis-password
{{- else -}}
{{- fail (printf "aibrix.redis.connectionPasswordKey: unknown component %q" .component) -}}
{{- end -}}
{{- end -}}
