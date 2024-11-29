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

package features

import (
	"fmt"
	"strings"
)

const (
	PodAutoscalerController        = "pod-autoscaler-controller"
	DistributedInferenceController = "distributed-inference-controller"
	ModelAdapterController         = "model-adapter-controller"
	ModelRouteController           = "model-route-controller"
)

var (
	// EnabledControllers is the map of controllers to enable or disable
	// '*' means "all enabled by default controllers"
	// 'foo' means "enable 'foo'"
	// '-foo' means "disable 'foo'"
	// first item for a particular name wins
	EnabledControllers = make(map[string]bool)

	ValidControllers = []string{
		PodAutoscalerController, DistributedInferenceController, ModelAdapterController, ModelRouteController,
	}
)

// ValidateControllers checks the list of controllers for any invalid entries.
func ValidateControllers(controllerList string) error {
	controllers := strings.Split(controllerList, ",")
	for _, controller := range controllers {
		trimmed := strings.TrimSpace(controller)
		if trimmed == "*" {
			// Skip wildcard since it's always valid
			continue
		}

		controllerName := trimmed
		if strings.HasPrefix(trimmed, "-") {
			controllerName = trimmed[1:] // Remove the '-' for validation
		}

		if !isValidController(controllerName) {
			return fmt.Errorf("invalid controller specified: %s", controllerName)
		}
	}
	return nil
}

// isValidController checks if the provided controller name is in the list of valid controllers.
func isValidController(name string) bool {
	for _, valid := range ValidControllers {
		if name == valid {
			return true
		}
	}
	return false
}

// InitControllers initializes the map of enabled controllers based on a comma-separated list.
func InitControllers(controllerList string) {
	controllers := strings.Split(controllerList, ",")
	for _, controller := range controllers {
		trimmed := strings.TrimSpace(controller)
		if trimmed == "*" {
			// Enable all controllers by default
			EnableAllControllers()
			continue
		}

		if strings.HasPrefix(trimmed, "-") {
			// Disable specified controller
			EnabledControllers[trimmed[1:]] = false
		} else {
			// Enable specified controller
			EnabledControllers[trimmed] = true
		}
	}
}

// IsControllerEnabled checks if a controller is enabled.
func IsControllerEnabled(name string) bool {
	enabled, exists := EnabledControllers[name]
	if !exists {
		return false // If not specified, consider it disabled to be safe.
	}
	return enabled
}

// EnableAllControllers is used to enable all known controllers.
func EnableAllControllers() {
	// This should be updated to reflect all available controllers.
	EnabledControllers[PodAutoscalerController] = true
	EnabledControllers[ModelAdapterController] = true
	EnabledControllers[DistributedInferenceController] = true
	EnabledControllers[ModelRouteController] = true
}
