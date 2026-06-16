/*
Copyright 2025 The Aibrix Team.

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

package error_injection

import (
	"strings"
	"testing"
)

func TestRenderError(t *testing.T) {
	tests := []struct {
		name        string
		template    *InjectionTemplate
		overrides   map[string]string
		wantErr     bool
		errContains string
		validate    func(t *testing.T, result *InjectedError)
	}{
		{
			name: "simple template without placeholders",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Request timed out",
				Placeholders:    nil,
			},
			overrides: nil,
			wantErr:   false,
			validate: func(t *testing.T, result *InjectedError) {
				if result.Type != ErrorTypeTimeout {
					t.Errorf("expected type %s, got %s", ErrorTypeTimeout, result.Type)
				}
				if result.Code != "DEADLINE_EXCEEDED" {
					t.Errorf("expected code DEADLINE_EXCEEDED, got %s", result.Code)
				}
				if result.Message != "Request timed out" {
					t.Errorf("expected message 'Request timed out', got %q", result.Message)
				}
			},
		},
		{
			name: "template with placeholders using defaults",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Provision timed out after {{.timeout_seconds}}s",
				Placeholders: map[string]string{
					"timeout_seconds": "60",
				},
			},
			overrides: nil,
			wantErr:   false,
			validate: func(t *testing.T, result *InjectedError) {
				expected := "Provision timed out after 60s"
				if result.Message != expected {
					t.Errorf("expected message %q, got %q", expected, result.Message)
				}
			},
		},
		{
			name: "template with overrides",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Provision timed out after {{.timeout_seconds}}s",
				Placeholders: map[string]string{
					"timeout_seconds": "60",
				},
			},
			overrides: map[string]string{
				"timeout_seconds": "120",
			},
			wantErr: false,
			validate: func(t *testing.T, result *InjectedError) {
				expected := "Provision timed out after 120s"
				if result.Message != expected {
					t.Errorf("expected message %q, got %q", expected, result.Message)
				}
			},
		},
		{
			name: "template with DetailsTemplate",
			template: &InjectionTemplate{
				Type:            ErrorTypeUnavailable,
				Code:            "RESOURCE_EXHAUSTED",
				MessageTemplate: "No {{.gpu_type}} GPUs available in {{.region}}",
				Placeholders: map[string]string{
					"gpu_type": "A100",
					"region":   "us-east-1",
				},
				DetailsTemplate: map[string]string{
					"gpu_type":     "{{.gpu_type}}",
					"region":       "{{.region}}",
					"suggestion":   "Try {{.region}}-west or increase timeout to {{.timeout_seconds}}s",
					"static_field": "This is a static value",
				},
			},
			overrides: map[string]string{
				"gpu_type":        "H100",
				"timeout_seconds": "300",
			},
			wantErr: false,
			validate: func(t *testing.T, result *InjectedError) {
				expectedMessage := "No H100 GPUs available in us-east-1"
				if result.Message != expectedMessage {
					t.Errorf("expected message %q, got %q", expectedMessage, result.Message)
				}
				if result.Details["gpu_type"] != "H100" {
					t.Errorf("expected details gpu_type 'H100', got %q", result.Details["gpu_type"])
				}
				if result.Details["region"] != "us-east-1" {
					t.Errorf("expected details region 'us-east-1', got %q", result.Details["region"])
				}
				expectedSuggestion := "Try us-east-1-west or increase timeout to 300s"
				if result.Details["suggestion"] != expectedSuggestion {
					t.Errorf("expected details suggestion %q, got %q", expectedSuggestion, result.Details["suggestion"])
				}
				if result.Details["static_field"] != "This is a static value" {
					t.Errorf("expected details static_field 'This is a static value', got %q", result.Details["static_field"])
				}
			},
		},
		{
			name:        "nil template handling",
			template:    nil,
			overrides:   nil,
			wantErr:     true,
			errContains: "template cannot be nil",
		},
		{
			name: "template with invalid template syntax",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Invalid {{.template",
				Placeholders:    map[string]string{},
			},
			overrides:   nil,
			wantErr:     true,
			errContains: "failed to render message template",
		},
		{
			name: "template with invalid details template syntax",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Valid message",
				Placeholders:    map[string]string{},
				DetailsTemplate: map[string]string{
					"bad": "Invalid {{.detail",
				},
			},
			overrides:   nil,
			wantErr:     true,
			errContains: "failed to render details template",
		},
		{
			name: "empty message template",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "",
				Placeholders:    nil,
			},
			overrides: nil,
			wantErr:   false,
			validate: func(t *testing.T, result *InjectedError) {
				if result.Message != "" {
					t.Errorf("expected empty message, got %q", result.Message)
				}
			},
		},
		{
			name: "empty DetailsTemplate",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Simple error",
				Placeholders:    nil,
				DetailsTemplate: map[string]string{},
			},
			overrides: nil,
			wantErr:   false,
			validate: func(t *testing.T, result *InjectedError) {
				if result.Details == nil {
					t.Error("expected details map to be initialized, got nil")
				}
				if len(result.Details) != 0 {
					t.Errorf("expected empty details map, got %d items", len(result.Details))
				}
			},
		},
		{
			name: "override adds new placeholder not in defaults",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Error with {{.custom}} value",
				Placeholders:    map[string]string{},
			},
			overrides: map[string]string{
				"custom": "special",
			},
			wantErr: false,
			validate: func(t *testing.T, result *InjectedError) {
				expected := "Error with special value"
				if result.Message != expected {
					t.Errorf("expected message %q, got %q", expected, result.Message)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := RenderError(tt.template, tt.overrides)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.validate != nil {
				tt.validate(t, result)
			}
		})
	}
}

func TestRenderMessage(t *testing.T) {
	tests := []struct {
		name        string
		templateStr string
		data        map[string]string
		want        string
		wantErr     bool
		errContains string
	}{
		{
			name:        "simple message rendering",
			templateStr: "Hello, World!",
			data:        nil,
			want:        "Hello, World!",
			wantErr:     false,
		},
		{
			name:        "template with single variable",
			templateStr: "Hello, {{.name}}!",
			data: map[string]string{
				"name": "Alice",
			},
			want:    "Hello, Alice!",
			wantErr: false,
		},
		{
			name:        "template with multiple variables",
			templateStr: "{{.greeting}}, {{.name}}! Welcome to {{.place}}.",
			data: map[string]string{
				"greeting": "Hello",
				"name":     "Bob",
				"place":    "Paris",
			},
			want:    "Hello, Bob! Welcome to Paris.",
			wantErr: false,
		},
		{
			name:        "empty template returns empty string",
			templateStr: "",
			data:        map[string]string{"key": "value"},
			want:        "",
			wantErr:     false,
		},
		{
			name:        "invalid template returns error - unclosed braces",
			templateStr: "Hello {{.name",
			data:        map[string]string{},
			wantErr:     true,
			errContains: "failed to parse template",
		},
		{
			name:        "invalid template returns error - incomplete braces",
			templateStr: "Hello {{.name }",
			data:        map[string]string{},
			wantErr:     true,
			errContains: "failed to parse template",
		},
		{
			name:        "missing variable renders as <no value>",
			templateStr: "Hello, {{.missing}}!",
			data:        map[string]string{},
			want:        "Hello, <no value>!",
			wantErr:     false,
		},
		{
			name:        "template with numbers",
			templateStr: "Count: {{.count}}, Total: {{.total}}",
			data: map[string]string{
				"count": "42",
				"total": "100",
			},
			want:    "Count: 42, Total: 100",
			wantErr: false,
		},
		{
			name:        "template with special characters",
			templateStr: "Path: {{.path}}, URL: {{.url}}",
			data: map[string]string{
				"path": "/usr/local/bin",
				"url":  "https://example.com/api?v=1",
			},
			want:    "Path: /usr/local/bin, URL: https://example.com/api?v=1",
			wantErr: false,
		},
		{
			name:        "nil data map",
			templateStr: "Static template",
			data:        nil,
			want:        "Static template",
			wantErr:     false,
		},
		{
			name:        "template with nested dots in field name",
			templateStr: "Value: {{.field}}",
			data: map[string]string{
				"field": "nested.value",
			},
			want:    "Value: nested.value",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderMessage(tt.templateStr, tt.data)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestMergePlaceholders(t *testing.T) {
	tests := []struct {
		name      string
		defaults  map[string]string
		overrides map[string]string
		want      map[string]string
	}{
		{
			name:      "empty defaults and overrides",
			defaults:  nil,
			overrides: nil,
			want:      map[string]string{},
		},
		{
			name: "defaults only",
			defaults: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			overrides: nil,
			want: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:      "overrides only",
			defaults:  nil,
			overrides: map[string]string{"key1": "override1"},
			want:      map[string]string{"key1": "override1"},
		},
		{
			name: "override takes precedence over defaults",
			defaults: map[string]string{
				"key1": "default1",
				"key2": "default2",
			},
			overrides: map[string]string{
				"key1": "override1",
				"key3": "override3",
			},
			want: map[string]string{
				"key1": "override1",
				"key2": "default2",
				"key3": "override3",
			},
		},
		{
			name: "original maps not mutated - defaults",
			defaults: map[string]string{
				"key1": "value1",
			},
			overrides: map[string]string{
				"key1": "override1",
			},
			want: map[string]string{
				"key1": "override1",
			},
		},
		{
			name: "empty string values preserved",
			defaults: map[string]string{
				"key1": "",
				"key2": "value2",
			},
			overrides: map[string]string{
				"key2": "",
			},
			want: map[string]string{
				"key1": "",
				"key2": "",
			},
		},
		{
			name: "multiple keys with mixed overlap",
			defaults: map[string]string{
				"a": "default_a",
				"b": "default_b",
				"c": "default_c",
			},
			overrides: map[string]string{
				"b": "override_b",
				"c": "override_c",
				"d": "override_d",
			},
			want: map[string]string{
				"a": "default_a",
				"b": "override_b",
				"c": "override_c",
				"d": "override_d",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Snapshot original maps for mutation check
			var origDefaults, origOverrides map[string]string
			if tt.defaults != nil {
				origDefaults = make(map[string]string)
				for k, v := range tt.defaults {
					origDefaults[k] = v
				}
			}
			if tt.overrides != nil {
				origOverrides = make(map[string]string)
				for k, v := range tt.overrides {
					origOverrides[k] = v
				}
			}

			got := MergePlaceholders(tt.defaults, tt.overrides)

			// Verify result
			if len(got) != len(tt.want) {
				t.Errorf("expected %d items, got %d", len(tt.want), len(got))
			}
			for k, wantV := range tt.want {
				gotV, exists := got[k]
				if !exists {
					t.Errorf("missing key %q in result", k)
					continue
				}
				if gotV != wantV {
					t.Errorf("key %q: expected %q, got %q", k, wantV, gotV)
				}
			}

			// Verify original maps not mutated
			if origDefaults != nil {
				for k, v := range origDefaults {
					if tt.defaults[k] != v {
						t.Errorf("defaults map was mutated: key %q was %q, now %q", k, v, tt.defaults[k])
					}
				}
			}
			if origOverrides != nil {
				for k, v := range origOverrides {
					if tt.overrides[k] != v {
						t.Errorf("overrides map was mutated: key %q was %q, now %q", k, v, tt.overrides[k])
					}
				}
			}
		})
	}
}

func TestValidateOverrides(t *testing.T) {
	tests := []struct {
		name        string
		template    *InjectionTemplate
		overrides   map[string]string
		wantErr     bool
		errContains string
	}{
		{
			name: "valid overrides - keys exist in template",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"timeout_seconds": "60",
					"region":          "us-east-1",
				},
			},
			overrides: map[string]string{
				"timeout_seconds": "120",
			},
			wantErr: false,
		},
		{
			name: "invalid overrides - keys don't exist",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"timeout_seconds": "60",
				},
			},
			overrides: map[string]string{
				"invalid_key": "value",
			},
			wantErr:     true,
			errContains: "invalid override keys",
		},
		{
			name: "empty overrides",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"key": "value",
				},
			},
			overrides: map[string]string{},
			wantErr:   false,
		},
		{
			name: "nil overrides",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"key": "value",
				},
			},
			overrides: nil,
			wantErr:   false,
		},
		{
			name:        "nil template",
			template:    nil,
			overrides:   map[string]string{"key": "value"},
			wantErr:     true,
			errContains: "template cannot be nil",
		},
		{
			name: "multiple invalid keys",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"valid_key": "value",
				},
			},
			overrides: map[string]string{
				"invalid1": "value1",
				"invalid2": "value2",
			},
			wantErr:     true,
			errContains: "invalid override keys",
		},
		{
			name: "mixed valid and invalid keys",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"valid_key": "value",
				},
			},
			overrides: map[string]string{
				"valid_key":   "new_value",
				"invalid_key": "value",
			},
			wantErr:     true,
			errContains: "invalid override keys",
		},
		{
			name: "template with empty placeholders",
			template: &InjectionTemplate{
				Placeholders: map[string]string{},
			},
			overrides: map[string]string{
				"any_key": "value",
			},
			wantErr:     true,
			errContains: "invalid override keys",
		},
		{
			name: "template with nil placeholders",
			template: &InjectionTemplate{
				Placeholders: nil,
			},
			overrides: map[string]string{
				"any_key": "value",
			},
			wantErr:     true,
			errContains: "invalid override keys",
		},
		{
			name: "all overrides are valid",
			template: &InjectionTemplate{
				Placeholders: map[string]string{
					"key1": "value1",
					"key2": "value2",
					"key3": "value3",
				},
			},
			overrides: map[string]string{
				"key1": "override1",
				"key3": "override3",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOverrides(tt.template, tt.overrides)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestTemplateExamples(t *testing.T) {
	tests := []struct {
		name        string
		template    *InjectionTemplate
		overrides   map[string]string
		wantMessage string
		wantErr     bool
	}{
		{
			name: "provision timeout example",
			template: &InjectionTemplate{
				Type:            ErrorTypeTimeout,
				Code:            "DEADLINE_EXCEEDED",
				MessageTemplate: "Provision timed out after {{.timeout_seconds}}s",
				Placeholders: map[string]string{
					"timeout_seconds": "60",
				},
			},
			overrides: map[string]string{
				"timeout_seconds": "120",
			},
			wantMessage: "Provision timed out after 120s",
			wantErr:     false,
		},
		{
			name: "GPU unavailable example",
			template: &InjectionTemplate{
				Type:            ErrorTypeUnavailable,
				Code:            "RESOURCE_EXHAUSTED",
				MessageTemplate: "No {{.gpu_type}} GPUs available in {{.region}}",
				Placeholders: map[string]string{
					"gpu_type": "A100",
					"region":   "us-east-1",
				},
			},
			overrides: map[string]string{
				"gpu_type": "H100",
				"region":   "us-west-2",
			},
			wantMessage: "No H100 GPUs available in us-west-2",
			wantErr:     false,
		},
		{
			name: "GPU unavailable with partial override",
			template: &InjectionTemplate{
				Type:            ErrorTypeUnavailable,
				Code:            "RESOURCE_EXHAUSTED",
				MessageTemplate: "No {{.gpu_type}} GPUs available in {{.region}}",
				Placeholders: map[string]string{
					"gpu_type": "A100",
					"region":   "us-east-1",
				},
			},
			overrides: map[string]string{
				"gpu_type": "H100",
			},
			wantMessage: "No H100 GPUs available in us-east-1",
			wantErr:     false,
		},
		{
			name: "complex error with details",
			template: &InjectionTemplate{
				Type:            ErrorTypeNotFound,
				Code:            "NOT_FOUND",
				MessageTemplate: "Model {{.model_name}} not found in namespace {{.namespace}}",
				Placeholders: map[string]string{
					"model_name": "default-model",
					"namespace":  "default",
				},
				DetailsTemplate: map[string]string{
					"suggestion": "Available models: {{.available_models}}",
					"namespace":  "{{.namespace}}",
				},
			},
			overrides: map[string]string{
				"model_name":       "custom-llm",
				"available_models": "gpt-4, claude-3, gemini",
			},
			wantMessage: "Model custom-llm not found in namespace default",
			wantErr:     false,
		},
		{
			name: "rate limit error",
			template: &InjectionTemplate{
				Type:            ErrorTypeUnavailable,
				Code:            "RESOURCE_EXHAUSTED",
				MessageTemplate: "Rate limit exceeded: {{.limit}} requests per {{.period}}. Retry after {{.retry_after}}s.",
				Placeholders: map[string]string{
					"limit":       "100",
					"period":      "minute",
					"retry_after": "60",
				},
			},
			overrides: map[string]string{
				"limit":       "1000",
				"retry_after": "30",
			},
			wantMessage: "Rate limit exceeded: 1000 requests per minute. Retry after 30s.",
			wantErr:     false,
		},
		{
			name: "invalid argument error with suggestions",
			template: &InjectionTemplate{
				Type:            ErrorTypeInvalidArgument,
				Code:            "INVALID_ARGUMENT",
				MessageTemplate: "Invalid parameter '{{.param}}': {{.reason}}",
				Placeholders: map[string]string{
					"param":  "unknown",
					"reason": "must be a valid value",
				},
			},
			overrides: map[string]string{
				"param":  "temperature",
				"reason": "must be between 0 and 2",
			},
			wantMessage: "Invalid parameter 'temperature': must be between 0 and 2",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := RenderError(tt.template, tt.overrides)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if result.Message != tt.wantMessage {
				t.Errorf("expected message %q, got %q", tt.wantMessage, result.Message)
			}
		})
	}
}

func TestValidateOverridesErrorMessage(t *testing.T) {
	template := &InjectionTemplate{
		Placeholders: map[string]string{
			"valid_key1": "value1",
			"valid_key2": "value2",
		},
	}
	overrides := map[string]string{
		"invalid_key1": "value1",
		"invalid_key2": "value2",
	}

	err := ValidateOverrides(template, overrides)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()

	// Check that error message contains invalid keys
	if !strings.Contains(errStr, "invalid_key1") || !strings.Contains(errStr, "invalid_key2") {
		t.Errorf("error message should contain both invalid keys, got: %q", errStr)
	}

	// Check that error message contains valid keys for reference
	if !strings.Contains(errStr, "valid_key1") || !strings.Contains(errStr, "valid_key2") {
		t.Errorf("error message should show valid keys, got: %q", errStr)
	}

	// Check that the error message structure is correct
	if !strings.Contains(errStr, "invalid override keys:") {
		t.Errorf("error message should start with 'invalid override keys:', got: %q", errStr)
	}

	if !strings.Contains(errStr, "(valid keys:") {
		t.Errorf("error message should contain valid keys section, got: %q", errStr)
	}
}

func TestRenderMessageWithEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		templateStr string
		data        map[string]string
		want        string
		wantErr     bool
	}{
		{
			name:        "template with whitespace around variable",
			templateStr: "Value: {{ .key }}",
			data:        map[string]string{"key": "value"},
			want:        "Value: value",
			wantErr:     false,
		},
		{
			name:        "template with newline",
			templateStr: "Line1\nLine2: {{.key}}",
			data:        map[string]string{"key": "value"},
			want:        "Line1\nLine2: value",
			wantErr:     false,
		},
		{
			name:        "template with tab",
			templateStr: "Key:\t{{.key}}",
			data:        map[string]string{"key": "value"},
			want:        "Key:\tvalue",
			wantErr:     false,
		},
		{
			name:        "template with backslash",
			templateStr: `Path: {{.path}}\file.txt`,
			data:        map[string]string{"path": "C:\\Users"},
			want:        `Path: C:\Users\file.txt`,
			wantErr:     false,
		},
		{
			name:        "template with quotes",
			templateStr: `Message: "{{.msg}}"`,
			data:        map[string]string{"msg": "Hello, World!"},
			want:        `Message: "Hello, World!"`,
			wantErr:     false,
		},
		{
			name:        "template with dollar sign",
			templateStr: "Price: ${{.price}}",
			data:        map[string]string{"price": "99.99"},
			want:        "Price: $99.99",
			wantErr:     false,
		},
		{
			name:        "template with percent sign",
			templateStr: "Progress: {{.percent}}%",
			data:        map[string]string{"percent": "85"},
			want:        "Progress: 85%",
			wantErr:     false,
		},
		{
			name:        "template with HTML-like content",
			templateStr: "<div>{{.content}}</div>",
			data:        map[string]string{"content": "Hello"},
			want:        "<div>Hello</div>",
			wantErr:     false,
		},
		{
			name:        "template with JSON-like content",
			templateStr: `{"key": "{{.value}}"}`,
			data:        map[string]string{"value": "test"},
			want:        `{"key": "test"}`,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderMessage(tt.templateStr, tt.data)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
