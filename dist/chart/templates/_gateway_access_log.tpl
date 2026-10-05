{{/* Envoy Gateway 1.2 access-log settings. Filter state is keyed by the full HTTP filter name. */}}
{{- define "aibrix.gateway.diagnosticAccessLog" -}}
{{- $filter := .Values.gateway.envoyProxy.accessLog.extProcFilterName | default (printf "envoy.filters.http.ext_proc/envoyextensionpolicy/%s/%s-gateway-plugins-extension-policy/extproc/0" .Release.Namespace (include "aibrix.fullname" .)) -}}
disable: false
settings:
  - format:
      type: JSON
      json:
        ":authority": "%REQ(:AUTHORITY)%"
        "bytes_received": "%BYTES_RECEIVED%"
        "bytes_sent": "%BYTES_SENT%"
        "connection_termination_details": "%CONNECTION_TERMINATION_DETAILS%"
        "downstream_local_address": "%DOWNSTREAM_LOCAL_ADDRESS%"
        "downstream_remote_address": "%DOWNSTREAM_REMOTE_ADDRESS%"
        "duration": "%DURATION%"
        "method": "%REQ(:METHOD)%"
        "protocol": "%PROTOCOL%"
        "requested_server_name": "%REQUESTED_SERVER_NAME%"
        "response_code": "%RESPONSE_CODE%"
        "response_code_details": "%RESPONSE_CODE_DETAILS%"
        "response_flags": "%RESPONSE_FLAGS%"
        "route_name": "%ROUTE_NAME%"
        "start_time": "%START_TIME%"
        "upstream_cluster": "%UPSTREAM_CLUSTER%"
        "upstream_host": "%UPSTREAM_HOST%"
        "upstream_local_address": "%UPSTREAM_LOCAL_ADDRESS%"
        "upstream_transport_failure_reason": "%UPSTREAM_TRANSPORT_FAILURE_REASON%"
        "user-agent": "%REQ(USER-AGENT)%"
        "x-envoy-origin-path": "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%"
        "x-envoy-upstream-service-time": "%RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)%"
        "x-forwarded-for": "%REQ(X-FORWARDED-FOR)%"
        "x-request-id": "%REQ(X-REQUEST-ID)%"
        traceparent: "%REQ(TRACEPARENT)%"
        start_time_precise: "%START_TIME(%Y-%m-%dT%H:%M:%S.%9fZ)%"
        request_duration_ms: "%REQUEST_DURATION%"
        request_tx_duration_ms: "%REQUEST_TX_DURATION%"
        response_duration_ms: "%RESPONSE_DURATION%"
        response_tx_duration_ms: "%RESPONSE_TX_DURATION%"
        upstream_connection_pool_ready_duration_ms: "%UPSTREAM_CONNECTION_POOL_READY_DURATION%"
        ext_proc_request_header_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:request_header_latency_us)%%" $filter | quote }}
        ext_proc_request_header_call_status: {{ printf "%%FILTER_STATE(%s:FIELD:request_header_call_status)%%" $filter | quote }}
        ext_proc_request_body_call_count: {{ printf "%%FILTER_STATE(%s:FIELD:request_body_call_count)%%" $filter | quote }}
        ext_proc_request_body_total_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:request_body_total_latency_us)%%" $filter | quote }}
        ext_proc_request_body_max_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:request_body_max_latency_us)%%" $filter | quote }}
        ext_proc_request_body_last_call_status: {{ printf "%%FILTER_STATE(%s:FIELD:request_body_last_call_status)%%" $filter | quote }}
        ext_proc_response_header_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:response_header_latency_us)%%" $filter | quote }}
        ext_proc_response_header_call_status: {{ printf "%%FILTER_STATE(%s:FIELD:response_header_call_status)%%" $filter | quote }}
        ext_proc_response_body_call_count: {{ printf "%%FILTER_STATE(%s:FIELD:response_body_call_count)%%" $filter | quote }}
        ext_proc_response_body_total_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:response_body_total_latency_us)%%" $filter | quote }}
        ext_proc_response_body_max_latency_us: {{ printf "%%FILTER_STATE(%s:FIELD:response_body_max_latency_us)%%" $filter | quote }}
        ext_proc_response_body_last_call_status: {{ printf "%%FILTER_STATE(%s:FIELD:response_body_last_call_status)%%" $filter | quote }}
    sinks:
      - type: File
        file:
          path: /dev/stdout
{{- end -}}
