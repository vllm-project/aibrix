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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
)

func TestGetTraceID(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		requestID   string
		want        string
	}{
		{
			name:        "Valid W3C traceparent",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			requestID:   "req-id-12345",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:        "Valid traceparent with leading/trailing spaces",
			traceparent: "   00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01   ",
			requestID:   "req-id-12345",
			want:        "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:        "Empty traceparent",
			traceparent: "",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Whitespace only traceparent",
			traceparent: "     ",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: missing parts",
			traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736", // 只有两部分，少于4部分
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: trace ID length not 32",
			traceparent: "00-shortid-00f067aa0ba902b7-01", // ID长度不对
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
		{
			name:        "Invalid format: completely random string",
			traceparent: "just-a-random-string-without-proper-format",
			requestID:   "fallback-req-id",
			want:        "fallback-req-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetTraceID(tt.traceparent, tt.requestID)
			if got != tt.want {
				t.Errorf("GetTraceID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFieldsToAttributes(t *testing.T) {
	tests := []struct {
		name   string
		fields []interface{}
		want   []attribute.KeyValue
	}{
		{
			name: "supported and fallback value types",
			fields: []interface{}{
				"string", "value",
				"int", 7,
				"int64", int64(8),
				"float64", 1.5,
				"duration", 1500 * time.Millisecond,
				"fallback", true,
				99, "numeric key",
			},
			want: []attribute.KeyValue{
				attribute.String("string", "value"),
				attribute.Int("int", 7),
				attribute.Int64("int64", 8),
				attribute.Float64("float64", 1.5),
				attribute.String("duration", "1.5s"),
				attribute.String("fallback", "true"),
				attribute.String("99", "numeric key"),
			},
		},
		{
			name:   "empty fields",
			fields: nil,
			want:   []attribute.KeyValue{},
		},
		{
			name:   "incomplete key value pair is ignored",
			fields: []interface{}{"complete", "value", "orphan"},
			want: []attribute.KeyValue{
				attribute.String("complete", "value"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fieldsToAttributes(tt.fields))
		})
	}
}
