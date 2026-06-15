/*
Copyright 2026 The Aibrix Team.

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
	"fmt"
	"time"
)

// ErrorType represents the category of error to inject.
type ErrorType string

const (
	// ErrorTypeTimeout indicates a timeout error.
	ErrorTypeTimeout ErrorType = "timeout"
	// ErrorTypeUnavailable indicates a service unavailable error.
	ErrorTypeUnavailable ErrorType = "unavailable"
	// ErrorTypeInvalidArgument indicates an invalid argument error.
	ErrorTypeInvalidArgument ErrorType = "invalid_argument"
	// ErrorTypeNotFound indicates a resource not found error.
	ErrorTypeNotFound ErrorType = "not_found"
	// ErrorTypePermissionDenied indicates a permission denied error.
	ErrorTypePermissionDenied ErrorType = "permission_denied"
	// ErrorTypeResourceExhausted indicates a resource exhausted error.
	ErrorTypeResourceExhausted ErrorType = "resource_exhausted"
	// ErrorTypeInternal indicates an internal error.
	ErrorTypeInternal ErrorType = "internal"
	// ErrorTypeCrash indicates a simulated crash/panic.
	ErrorTypeCrash ErrorType = "crash"
)

// InjectionTemplate defines a template for generating specific error types.
type InjectionTemplate struct {
	// Type is the category of error this template produces.
	Type ErrorType `json:"type"`
	// Code is the gRPC/HTTP status code for the error.
	Code string `json:"code"`
	// MessageTemplate is a Go template string with {{.variable}} syntax for error message.
	MessageTemplate string `json:"message_template"`
	// Placeholders contains template variables with their default values.
	Placeholders map[string]string `json:"placeholders,omitempty"`
	// DetailsTemplate contains optional structured error details templates.
	DetailsTemplate map[string]string `json:"details_template,omitempty"`
}

// InjectionPoint represents a specific point in the pipeline where errors can be injected.
type InjectionPoint struct {
	// ID is a unique identifier in the format "component.action".
	ID string `json:"id"`
	// Component is the pipeline component name.
	Component string `json:"component"`
	// Action is the specific operation within the component.
	Action string `json:"action"`
	// Description provides a human-readable explanation of the injection point.
	Description string `json:"description"`
	// Templates contains pre-defined error templates for this injection point.
	Templates map[ErrorType]*InjectionTemplate `json:"templates,omitempty"`
}

// InjectionRule defines a rule for injecting errors at specific points.
type InjectionRule struct {
	// PointRef references the injection point by ID.
	PointRef string `json:"point_ref"`
	// ErrorType specifies which template to use from the injection point.
	ErrorType ErrorType `json:"error_type"`
	// Probability is the chance (0.0-1.0) that the error will trigger.
	Probability float64 `json:"probability"`
	// Overrides allows customization of template placeholder values.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// InjectedError represents an error that was injected into the pipeline.
type InjectedError struct {
	// Type is the category of the injected error.
	Type ErrorType `json:"type"`
	// Code is the gRPC/HTTP status code.
	Code string `json:"code"`
	// Message is the rendered error description.
	Message string `json:"message"`
	// Details contains additional structured error information.
	Details map[string]string `json:"details,omitempty"`
}

// ToError converts the InjectedError to a standard Go error.
func (e *InjectedError) ToError() error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("injected error [%s]: %s (code: %s)", e.Type, e.Message, e.Code)
}

// Error implements the error interface for InjectedError.
func (e *InjectedError) Error() string {
	if e == nil {
		return ""
	}
	return e.ToError().Error()
}

// InjectionConfig contains per-job error injection configuration.
type InjectionConfig struct {
	// JobID is the identifier of the job this config applies to.
	JobID string `json:"job_id"`
	// Enabled determines whether error injection is active for this job.
	Enabled bool `json:"enabled"`
	// Rules contains the injection rules to apply.
	Rules []InjectionRule `json:"rules,omitempty"`
	// GlobalProbability is the default probability for rules without explicit probability.
	GlobalProbability float64 `json:"global_probability,omitempty"`
}

// PointRecord tracks the execution and outcome of an injection point evaluation.
type PointRecord struct {
	// PointID is the identifier of the injection point that was evaluated.
	PointID string `json:"point_id"`
	// Timestamp is when the injection point was evaluated.
	Timestamp time.Time `json:"timestamp"`
	// Triggered indicates whether an error was actually injected.
	Triggered bool `json:"triggered"`
	// ContextSnapshot captures the relevant context at the time of evaluation.
	ContextSnapshot map[string]string `json:"context_snapshot,omitempty"`
	// Error contains the injected error if one was triggered.
	Error *InjectedError `json:"error,omitempty"`
	// TemplateUsed is the error type of the template that was applied.
	TemplateUsed ErrorType `json:"template_used,omitempty"`
	// OverridesApplied contains the placeholder overrides that were used.
	OverridesApplied map[string]string `json:"overrides_applied,omitempty"`
	// ProbabilityRoll is the random value that was rolled against the probability.
	ProbabilityRoll float64 `json:"probability_roll"`
}

// ExecutionTrace captures the complete trace of error injection for a job execution.
type ExecutionTrace struct {
	// JobID is the identifier of the job being traced.
	JobID string `json:"job_id"`
	// StartTime is when the job execution began.
	StartTime time.Time `json:"start_time"`
	// EndTime is when the job execution completed.
	EndTime time.Time `json:"end_time,omitempty"`
	// Points contains records for all injection points evaluated during execution.
	Points []PointRecord `json:"points,omitempty"`
}

// AppendPoint adds a new point record to the execution trace.
func (t *ExecutionTrace) AppendPoint(record PointRecord) {
	if t.Points == nil {
		t.Points = make([]PointRecord, 0)
	}
	t.Points = append(t.Points, record)
}

func (t *ExecutionTrace) Clone() *ExecutionTrace {
	// Return a copy of the trace to prevent concurrent read/write data races
	pointsCopy := make([]PointRecord, len(t.Points))
	copy(pointsCopy, t.Points)

	return &ExecutionTrace{
		JobID:     t.JobID,
		StartTime: t.StartTime,
		EndTime:   t.EndTime,
		Points:    pointsCopy,
	}
}

// GlobalInjectionConfig contains global error injection settings.
type GlobalInjectionConfig struct {
	// Enabled determines whether global error injection is active.
	Enabled bool `json:"enabled"`
	// Rules contains the global injection rules to apply.
	Rules []InjectionRule `json:"rules,omitempty"`
	// ExcludedPoints lists injection points that should never have errors injected.
	ExcludedPoints []string `json:"excluded_points,omitempty"`
	// GlobalProbability is the default probability for all injection points in chaos mode.
	GlobalProbability float64 `json:"global_probability,omitempty"`
	// PointWeights overrides probability per injection point in chaos mode.
	// Keys are point IDs (e.g., "rm.provision"), values are probabilities (0.0-1.0).
	// Point-specific probability takes precedence over GlobalProbability.
	PointWeights map[string]float64 `json:"point_weights,omitempty"`
}
