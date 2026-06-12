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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Sentinel errors for the injector.
var (
	// ErrInjectorDisabled is returned when the injector is disabled.
	ErrInjectorDisabled = errors.New("error injection is disabled")
	// ErrInvalidMode is returned when an invalid mode is specified.
	ErrInvalidMode = errors.New("invalid injector mode")
	// ErrPointNotFound is returned when an injection point is not found.
	ErrPointNotFound = errors.New("injection point not found")
	// ErrInvalidProbability is returned when probability is out of range.
	ErrInvalidProbability = errors.New("probability must be between 0.0 and 1.0")
	// ErrPointAlreadyRegistered is returned when registering a duplicate point.
	ErrPointAlreadyRegistered = errors.New("injection point already registered")
)

// Injector defines the interface for error injection.
type Injector interface {
	// CheckPoint checks if an error should be injected at the given point.
	// Returns nil if no error should be injected, or an InjectedError if one should be.
	CheckPoint(ctx context.Context, pointID string) error

	// GetTrace retrieves the execution trace for a job.
	GetTrace(ctx context.Context, jobID string) *ExecutionTrace

	// GetGlobalConfig returns the current global configuration.
	GetGlobalConfig() *GlobalInjectionConfig

	// SetGlobalConfig updates the global configuration.
	SetGlobalConfig(config *GlobalInjectionConfig) error

	// RenderTemplate renders an error from the template for a given point.
	RenderTemplate(pointID string, errorType ErrorType, overrides map[string]string) (*InjectedError, error)
}

// InjectorImpl implements the Injector interface.
type InjectorImpl struct {
	// globalConfig is the global injection configuration.
	globalConfig *GlobalInjectionConfig

	// globalConfigMu protects the globalConfig.
	globalConfigMu sync.RWMutex

	// rand is the random source for probability calculations.
	rand *rand.Rand

	// randMu protects the random source.
	randMu sync.Mutex

	// traceStore stores execution traces.
	traceStore TraceStore
}

// NewInjector creates a new InjectorImpl instance.
func NewInjector() (*InjectorImpl, error) {
	return &InjectorImpl{
		globalConfig: &GlobalInjectionConfig{
			Enabled:        false,
			Rules:          []InjectionRule{},
			ExcludedPoints: []string{},
		},
		rand:       rand.New(rand.NewSource(time.Now().UTC().UnixNano())),
		traceStore: NewInMemoryTraceStore(),
	}, nil
}

// CheckPoint checks if an error should be injected at the given point.
func (i *InjectorImpl) CheckPoint(ctx context.Context, pointID string) error {
	klog.Infof("[injection] CheckPoint: %s", pointID)
	jobConfig := GetInjectionConfigFromContext(ctx)
	jobID := ""
	if jobConfig != nil {
		jobID = jobConfig.JobID
	}

	// Get the injection point
	point := i.getPoint(pointID)
	if point == nil {
		klog.Warningf("[injection] Injection point %s not found, not injecting", pointID)
		return nil // Unknown point, no injection
	}

	// Record point visit to trace
	roll := i.rollRandom()
	record := PointRecord{
		PointID:         pointID,
		Timestamp:       time.Now().UTC(),
		Triggered:       false,
		ProbabilityRoll: roll,
	}
	recordJson, _ := json.Marshal(record)
	klog.Infof("[injection] Injection record: %s", recordJson)

	defer func() {
		if jobID != "" {
			if err := i.traceStore.AppendPoint(jobID, record); err != nil {
				klog.Errorf("[injection] Failed to append point to trace: %v", err)
			} else {
				klog.Infof("[injection] Successfully appended point to trace: %s", recordJson)
			}
		}
	}()

	// Check if point is globally excluded
	globalConfig := i.getGlobalConfig()
	if i.isPointExcluded(pointID, globalConfig.ExcludedPoints) {
		klog.Infof("[injection] Injection point %s is globally excluded, not injecting", pointID)
		return nil
	}

	// Priority 1: Per-job config from context
	if jobConfig != nil && jobConfig.Enabled {
		if err := i.checkWithConfig(ctx, point, jobConfig.Rules, jobConfig.GlobalProbability, nil, &record); err != nil {
			klog.Infof("[injection] Per-job injection config check passed, injecting: %s", pointID)
			return err
		} else {
			klog.Infof("[injection] Per-job injection config check failed, not injecting: %s", pointID)
		}
	}

	// Priority 2: Global config
	if globalConfig.Enabled {
		if err := i.checkWithConfig(ctx, point, globalConfig.Rules, globalConfig.GlobalProbability, globalConfig.PointWeights, &record); err != nil {
			klog.Infof("[injection] Global injection config check passed, injecting: %s", pointID)
			return err
		} else {
			klog.Infof("[injection] Global injection config check failed, not injecting: %s", pointID)
		}
	}

	// Priority 3: No injection
	return nil
}

// checkWithConfig checks for injection based on the given configuration.
func (i *InjectorImpl) checkWithConfig(
	ctx context.Context,
	point *InjectionPoint,
	rules []InjectionRule,
	globalProbability float64,
	pointWeights map[string]float64,
	record *PointRecord,
) error {
	// Find matching rule (highest priority first, using order in slice)
	rule := i.findRule(point, rules)
	if rule == nil {
		return nil
	}

	// Determine probability with priority:
	// 1. Rule-specific probability (highest priority)
	// 2. Point-specific weight from PointWeights (chaos mode)
	// 3. Global probability (fallback)
	probability := rule.Probability
	if probability == 0.0 {
		if pointWeights != nil && pointWeights[point.ID] > 0.0 {
			probability = pointWeights[point.ID]
		} else if globalProbability > 0.0 {
			probability = globalProbability
		}
	}

	// Roll random and check probability
	roll := record.ProbabilityRoll
	if roll > probability {
		return nil
	}

	// Render error from template
	injectedErr, err := i.RenderTemplate(point.ID, rule.ErrorType, rule.Overrides)
	if err != nil {
		klog.Errorf("[injection] Failed to render template for point %s, error type %s: %v", point.ID, rule.ErrorType, err)
		return nil // Don't inject on render error
	}

	// Update record with triggered info
	record.Triggered = true
	record.Error = injectedErr
	record.TemplateUsed = rule.ErrorType
	record.OverridesApplied = rule.Overrides

	// Handle crash type - panic instead of returning error
	if injectedErr.Type == ErrorTypeCrash {
		klog.Errorf("[injection] Simulating crash at %s: %s", point.ID, injectedErr.Message)
		panic(injectedErr.Message)
	}

	return injectedErr
}

// findRule finds the first matching rule for the point (rules are evaluated in order).
func (i *InjectorImpl) findRule(point *InjectionPoint, rules []InjectionRule) *InjectionRule {
	for idx := range rules {
		rule := &rules[idx]
		// Check if rule matches this point
		if rule.PointRef == point.ID || i.matchesPattern(rule.PointRef, point.ID) {
			return rule
		}
	}
	return nil
}

// matchesPattern checks if the pointRef pattern matches the pointID.
// Supports simple wildcard patterns like "component.*" or "*".
func (i *InjectorImpl) matchesPattern(pattern, pointID string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(pointID, prefix+".")
	}
	return false
}

// isPointExcluded checks if a point is in the excluded list.
func (i *InjectorImpl) isPointExcluded(pointID string, excluded []string) bool {
	for _, ex := range excluded {
		if ex == pointID {
			return true
		}
	}
	return false
}

func (i *InjectorImpl) GetTrace(ctx context.Context, jobID string) *ExecutionTrace {
	trace, err := i.traceStore.Get(jobID)
	if err != nil {
		klog.Errorf("[injection] failed to get trace for job %s: %v", jobID, err)
		return nil
	}
	return trace
}

// GetGlobalConfig returns the current global configuration.
func (i *InjectorImpl) GetGlobalConfig() *GlobalInjectionConfig {
	return i.getGlobalConfig()
}

// getGlobalConfig returns the current global configuration (thread-safe helper).
func (i *InjectorImpl) getGlobalConfig() *GlobalInjectionConfig {
	i.globalConfigMu.RLock()
	defer i.globalConfigMu.RUnlock()
	return i.globalConfig
}

// SetGlobalConfig updates the global configuration.
func (i *InjectorImpl) SetGlobalConfig(config *GlobalInjectionConfig) error {
	if config == nil {
		return errors.New("config cannot be nil")
	}

	i.globalConfigMu.Lock()
	defer i.globalConfigMu.Unlock()

	i.globalConfig = config
	return nil
}

// RenderTemplate renders an error from the template for a given point.
func (i *InjectorImpl) RenderTemplate(pointID string, errorType ErrorType, overrides map[string]string) (*InjectedError, error) {
	point := i.getPoint(pointID)
	if point == nil {
		return nil, fmt.Errorf("%w: %s", ErrPointNotFound, pointID)
	}

	// Get the template for this error type
	template, ok := point.Templates[errorType]
	if !ok {
		// Create a default template if none exists
		template = &InjectionTemplate{
			Type:            errorType,
			Code:            getDefaultCode(errorType),
			MessageTemplate: fmt.Sprintf("injected %s error at %s", errorType, pointID),
			Placeholders:    make(map[string]string),
			DetailsTemplate: make(map[string]string),
		}
	}

	// Use the existing RenderError function from template.go
	injectedErr, err := RenderError(template, overrides)
	if err != nil {
		return nil, fmt.Errorf("failed to render error: %w", err)
	}

	return injectedErr, nil
}

// getDefaultCode returns the default gRPC/HTTP code for an error type.
func getDefaultCode(errorType ErrorType) string {
	switch errorType {
	case ErrorTypeTimeout:
		return "DEADLINE_EXCEEDED"
	case ErrorTypeUnavailable:
		return "UNAVAILABLE"
	case ErrorTypeInvalidArgument:
		return "INVALID_ARGUMENT"
	case ErrorTypeNotFound:
		return "NOT_FOUND"
	case ErrorTypePermissionDenied:
		return "PERMISSION_DENIED"
	case ErrorTypeResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case ErrorTypeInternal:
		return "INTERNAL"
	case ErrorTypeCrash:
		return "CRASH"
	default:
		return "UNKNOWN"
	}
}

// getPoint retrieves an injection point by ID (thread-safe helper).
func (i *InjectorImpl) getPoint(pointID string) *InjectionPoint {
	return DefaultRegistry[pointID]
}

// rollRandom returns a random float64 between 0.0 and 1.0.
func (i *InjectorImpl) rollRandom() float64 {
	i.randMu.Lock()
	defer i.randMu.Unlock()
	return i.rand.Float64()
}

// GetPoint retrieves a registered injection point by ID.
func (i *InjectorImpl) GetPoint(pointID string) *InjectionPoint {
	return i.getPoint(pointID)
}

// ListPoints returns all registered injection points.
func (i *InjectorImpl) ListPoints() []*InjectionPoint {
	points := make([]*InjectionPoint, 0, len(DefaultRegistry))
	for _, point := range DefaultRegistry {
		points = append(points, point)
	}

	return points
}
