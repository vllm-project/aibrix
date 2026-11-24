/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

func buildGatewayPodMetricLabels(model, status, statusCode string) map[string]string {
	return map[string]string{
		"model":       GetModelTag(model),
		"status":      status,
		"status_code": statusCode,
		"pod_name":    POD_NAME,
	}
}

func GetModelTag(model string) string {
	if model == "" {
		return "unknown"
	}
	return model
}

func GetTraceID(traceparent, requestID string) string {
	traceparent = strings.TrimSpace(traceparent)
	if traceparent != "" {
		// W3C standard: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
		parts := strings.Split(traceparent, "-")
		if len(parts) == 4 && len(parts[1]) == 32 {
			return parts[1]
		}
	}
	return requestID
}

// fieldsToAttributes trans Key-Value to OTel attr
func fieldsToAttributes(fields []interface{}) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(fields)/2)
	for i := 0; i < len(fields)-1; i += 2 {
		key := fmt.Sprint(fields[i])

		switch v := fields[i+1].(type) {
		case string:
			attrs = append(attrs, attribute.String(key, v))
		case int:
			attrs = append(attrs, attribute.Int(key, v))
		case int64:
			attrs = append(attrs, attribute.Int64(key, v))
		case float64:
			attrs = append(attrs, attribute.Float64(key, v))
		case time.Duration:
			attrs = append(attrs, attribute.String(key, v.String()))
		default:
			attrs = append(attrs, attribute.String(key, fmt.Sprint(v)))
		}
	}
	return attrs
}
