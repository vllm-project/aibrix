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

package config

import (
	"fmt"
	"strconv"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/algorithm"
	"k8s.io/klog/v2"
)

// ConfigExtractor handles extraction and validation of configurations from PodAutoscaler
type ConfigExtractor struct {
	// Default values
	DefaultStableWindow   time.Duration
	DefaultPanicWindow    time.Duration
	DefaultWindow         time.Duration
	DefaultGranularity    time.Duration
	DefaultPanicThreshold float64
}

// NewConfigExtractor creates a new configuration extractor with defaults
func NewConfigExtractor() *ConfigExtractor {
	return &ConfigExtractor{
		DefaultStableWindow:   60 * time.Second,
		DefaultPanicWindow:    6 * time.Second,
		DefaultWindow:         60 * time.Second,
		DefaultGranularity:    time.Second,
		DefaultPanicThreshold: 2.0,
	}
}

// ExtractAlgorithmConfig extracts and validates algorithm configuration
func (e *ConfigExtractor) ExtractAlgorithmConfig(pa autoscalingv1alpha1.PodAutoscaler) (algorithm.AlgorithmConfig, error) {
	config := algorithm.AlgorithmConfig{
		Strategy:       pa.Spec.ScalingStrategy,
		PanicThreshold: e.DefaultPanicThreshold,
		StableWindow:   e.DefaultStableWindow,
		PanicWindow:    e.DefaultPanicWindow,
		ScaleToZero:    false,
	}

	if pa.Annotations == nil {
		return config, nil
	}

	// Parse stable window duration
	if stableWindowStr, ok := pa.Annotations["autoscaling.aibrix.ai/stable-window"]; ok {
		duration, err := time.ParseDuration(stableWindowStr)
		if err != nil {
			klog.V(4).InfoS("Failed to parse stable window duration", "value", stableWindowStr, "error", err)
		} else if err := e.validateDuration("stable-window", duration, time.Second, 10*time.Minute); err != nil {
			return config, fmt.Errorf("invalid stable window: %w", err)
		} else {
			config.StableWindow = duration
		}
	}

	// Parse panic window duration
	if panicWindowStr, ok := pa.Annotations["autoscaling.aibrix.ai/panic-window"]; ok {
		duration, err := time.ParseDuration(panicWindowStr)
		if err != nil {
			klog.V(4).InfoS("Failed to parse panic window duration", "value", panicWindowStr, "error", err)
		} else if err := e.validateDuration("panic-window", duration, time.Second, config.StableWindow); err != nil {
			return config, fmt.Errorf("invalid panic window: %w", err)
		} else {
			config.PanicWindow = duration
		}
	}

	// Validate relationships
	if config.PanicWindow > config.StableWindow {
		return config, fmt.Errorf("panic window (%v) cannot be larger than stable window (%v)",
			config.PanicWindow, config.StableWindow)
	}

	// Parse panic threshold
	if thresholdStr, ok := pa.Annotations["autoscaling.aibrix.ai/panic-threshold"]; ok {
		threshold, err := strconv.ParseFloat(thresholdStr, 64)
		if err != nil {
			klog.V(4).InfoS("Failed to parse panic threshold", "value", thresholdStr, "error", err)
		} else if threshold < 1.0 {
			return config, fmt.Errorf("panic threshold must be >= 1.0, got %f", threshold)
		} else if threshold > 10.0 {
			return config, fmt.Errorf("panic threshold must be <= 10.0, got %f", threshold)
		} else {
			config.PanicThreshold = threshold
		}
	}

	// Parse scale to zero flag
	if scaleToZeroStr, ok := pa.Annotations["autoscaling.aibrix.ai/scale-to-zero"]; ok {
		scaleToZero, err := strconv.ParseBool(scaleToZeroStr)
		if err != nil {
			klog.V(4).InfoS("Failed to parse scale-to-zero flag", "value", scaleToZeroStr, "error", err)
		} else {
			config.ScaleToZero = scaleToZero
		}
	}

	// Validate scale to zero with min replicas
	if config.ScaleToZero && pa.Spec.MinReplicas != nil && *pa.Spec.MinReplicas > 0 {
		klog.V(4).InfoS("Scale to zero enabled but min replicas > 0, disabling scale to zero",
			"minReplicas", *pa.Spec.MinReplicas)
		config.ScaleToZero = false
	}

	return config, nil
}

// ValidateScalingConstraints validates min/max replica constraints
func (e *ConfigExtractor) ValidateScalingConstraints(minReplicas, maxReplicas int32) error {
	if minReplicas < 0 {
		return fmt.Errorf("min replicas cannot be negative: %d", minReplicas)
	}
	if maxReplicas < 1 {
		return fmt.Errorf("max replicas must be at least 1: %d", maxReplicas)
	}
	if minReplicas > maxReplicas {
		return fmt.Errorf("min replicas (%d) cannot be greater than max replicas (%d)",
			minReplicas, maxReplicas)
	}
	return nil
}

// validateDuration validates a duration is within acceptable bounds
func (e *ConfigExtractor) validateDuration(name string, value, min, max time.Duration) error {
	if value < min {
		return fmt.Errorf("%s duration %v is less than minimum %v", name, value, min)
	}
	if value > max {
		return fmt.Errorf("%s duration %v is greater than maximum %v", name, value, max)
	}
	return nil
}
