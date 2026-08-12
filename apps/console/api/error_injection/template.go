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
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// RenderError generates an InjectedError from a template with placeholder overrides.
// It merges default placeholders with overrides, then renders the message and details templates.
func RenderError(template *InjectionTemplate, overrides map[string]string) (*InjectedError, error) {
	if template == nil {
		return nil, fmt.Errorf("template cannot be nil")
	}

	// Merge placeholders with overrides
	data := MergePlaceholders(template.Placeholders, overrides)

	// Render message template
	message, err := RenderMessage(template.MessageTemplate, data)
	if err != nil {
		return nil, fmt.Errorf("failed to render message template: %w", err)
	}

	// Render details templates
	details := make(map[string]string)
	for key, tmplStr := range template.DetailsTemplate {
		rendered, err := RenderMessage(tmplStr, data)
		if err != nil {
			return nil, fmt.Errorf("failed to render details template for key %q: %w", key, err)
		}
		details[key] = rendered
	}

	return &InjectedError{
		Type:    template.Type,
		Code:    template.Code,
		Message: message,
		Details: details,
	}, nil
}

// RenderMessage renders a single template string with the provided data.
// It uses Go's text/template package with {{.variable}} syntax.
func RenderMessage(templateStr string, data map[string]string) (string, error) {
	if templateStr == "" {
		return "", nil
	}

	// Parse the template
	tmpl, err := template.New("message").Parse(templateStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %q: %w", templateStr, err)
	}

	// Execute the template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %q: %w", templateStr, err)
	}

	return buf.String(), nil
}

// MergePlaceholders merges two placeholder maps with overrides taking precedence.
// Returns a new map without mutating the inputs.
func MergePlaceholders(defaults, overrides map[string]string) map[string]string {
	result := make(map[string]string)

	// Copy defaults first
	for k, v := range defaults {
		result[k] = v
	}

	// Apply overrides (takes precedence)
	for k, v := range overrides {
		result[k] = v
	}

	return result
}

// ValidateOverrides checks that all override keys exist in the template placeholders.
// Returns an error listing all invalid keys if any are found.
func ValidateOverrides(template *InjectionTemplate, overrides map[string]string) error {
	if template == nil {
		return fmt.Errorf("template cannot be nil")
	}

	if len(overrides) == 0 {
		return nil
	}

	// Collect invalid keys
	var invalidKeys []string
	for key := range overrides {
		if _, exists := template.Placeholders[key]; !exists {
			invalidKeys = append(invalidKeys, key)
		}
	}

	if len(invalidKeys) > 0 {
		sort.Strings(invalidKeys)
		return fmt.Errorf("invalid override keys: %s (valid keys: %v)",
			strings.Join(invalidKeys, ", "), sortedKeys(template.Placeholders))
	}

	return nil
}

// sortedKeys returns the keys of a map as a sorted slice for error messages
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
