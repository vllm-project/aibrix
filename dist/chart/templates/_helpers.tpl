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
{{- $metadata := get .Values "metadata" | default dict -}}
{{- $metadataRedis := get $metadata "redis" | default dict -}}
{{- $metadataService := get $metadata "service" | default dict -}}
{{- $metadataServiceRedis := get $metadataService "redis" | default dict -}}
{{- $gatewayPlugin := get .Values "gatewayPlugin" | default dict -}}
{{- $gatewayDependencies := get $gatewayPlugin "dependencies" | default dict -}}
{{- $gatewayRedis := get $gatewayDependencies "redis" | default dict -}}
{{- $gpuOptimizer := get .Values "gpuOptimizer" | default dict -}}
{{- $gpuDependencies := get $gpuOptimizer "dependencies" | default dict -}}
{{- $gpuRedis := get $gpuDependencies "redis" | default dict -}}
{{- $builtInRedisEnabled := true -}}
{{- if hasKey $metadataRedis "enabled" -}}
{{- $builtInRedisEnabled = get $metadataRedis "enabled" -}}
{{- end -}}
{{- $sharedEnablePassword := get $metadataRedis "enablePassword" | default false -}}
{{- $sharedPassword := get $metadataRedis "password" | default "" -}}
{{- if and $sharedEnablePassword (empty (trim $sharedPassword)) -}}
{{- fail "metadata.redis.enablePassword=true requires a non-empty metadata.redis.password." -}}
{{- end -}}
{{- if not $builtInRedisEnabled -}}
{{- $missingHosts := list -}}
{{- if empty (trim (get $metadataServiceRedis "host" | default "")) -}}
{{- $missingHosts = append $missingHosts "metadata.service.redis.host" -}}
{{- end -}}
{{- if empty (trim (get $gatewayRedis "host" | default "")) -}}
{{- $missingHosts = append $missingHosts "gatewayPlugin.dependencies.redis.host" -}}
{{- end -}}
{{- if empty (trim (get $gpuRedis "host" | default "")) -}}
{{- $missingHosts = append $missingHosts "gpuOptimizer.dependencies.redis.host" -}}
{{- end -}}
{{- if gt (len $missingHosts) 0 -}}
{{- fail (printf "metadata.redis.enabled=false requires non-empty values for %s." (join ", " $missingHosts)) -}}
{{- end -}}
{{- end -}}
{{- if and $builtInRedisEnabled (or
  (get $metadataServiceRedis "password" | default "")
  (get $gatewayRedis "password" | default "")
  (get $gpuRedis "password" | default "")) -}}
{{- fail "built-in Redis does not support component-specific passwords; use metadata.redis.enablePassword/password for shared built-in Redis auth or disable built-in Redis for external Redis passwords." -}}
{{- end -}}
{{- end -}}

{{/*
Return whether the shared Redis password config is enabled.
*/}}
{{- define "aibrix.redis.sharedHasPassword" -}}
{{- $metadata := get .Values "metadata" | default dict -}}
{{- $redis := get $metadata "redis" | default dict -}}
{{- if or (get $redis "enablePassword" | default false) (get $redis "password" | default "") -}}true{{- end -}}
{{- end -}}

{{/*
Return whether a component Redis connection should use password auth.
Supported components: metadataService, gatewayPlugin, gpuOptimizer.
*/}}
{{- define "aibrix.redis.connectionHasPassword" -}}
{{- $values := .Values -}}
{{- $component := .component -}}
{{- $metadata := get $values "metadata" | default dict -}}
{{- $metadataRedis := get $metadata "redis" | default dict -}}
{{- $sharedEnabled := get $metadataRedis "enablePassword" | default false -}}
{{- $sharedPassword := get $metadataRedis "password" | default "" -}}
{{- $componentPassword := "" -}}
{{- if eq $component "metadataService" -}}
  {{- $metadataService := get $metadata "service" | default dict -}}
  {{- $metadataServiceRedis := get $metadataService "redis" | default dict -}}
  {{- $componentPassword = get $metadataServiceRedis "password" | default "" -}}
{{- else if eq $component "gatewayPlugin" -}}
  {{- $gatewayPlugin := get $values "gatewayPlugin" | default dict -}}
  {{- $dependencies := get $gatewayPlugin "dependencies" | default dict -}}
  {{- $redis := get $dependencies "redis" | default dict -}}
  {{- $componentPassword = get $redis "password" | default "" -}}
{{- else if eq $component "gpuOptimizer" -}}
  {{- $gpuOptimizer := get $values "gpuOptimizer" | default dict -}}
  {{- $dependencies := get $gpuOptimizer "dependencies" | default dict -}}
  {{- $redis := get $dependencies "redis" | default dict -}}
  {{- $componentPassword = get $redis "password" | default "" -}}
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
