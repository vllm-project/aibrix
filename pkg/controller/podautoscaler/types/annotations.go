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

package types

// Annotation keys for PodAutoscaler configuration
// These constants define the annotation keys used to configure autoscaling behavior
const (
	// AutoscalingLabelPrefix is the prefix for all autoscaling annotation keys
	AutoscalingLabelPrefix = "autoscaling.aibrix.ai/"

	// MaxScaleUpRateLabel controls the maximum rate at which to scale up
	// Value: float (e.g., "2.0" means can double replicas in one step)
	MaxScaleUpRateLabel = AutoscalingLabelPrefix + "max-scale-up-rate"

	// MaxScaleDownRateLabel controls the maximum rate at which to scale down
	// Value: float (e.g., "2.0" means can halve replicas in one step)
	MaxScaleDownRateLabel = AutoscalingLabelPrefix + "max-scale-down-rate"

	// ScaleUpToleranceLabel sets tolerance for upward metric fluctuations
	// Value: float (e.g., "0.1" means 10% tolerance before scaling up)
	ScaleUpToleranceLabel = AutoscalingLabelPrefix + "scale-up-tolerance"

	// ScaleDownToleranceLabel sets tolerance for downward metric fluctuations
	// Value: float (e.g., "0.1" means 10% tolerance before scaling down)
	ScaleDownToleranceLabel = AutoscalingLabelPrefix + "scale-down-tolerance"

	// PanicThresholdLabel sets the threshold for entering panic mode (KPA only)
	// Value: float (e.g., "2.0" means panic when short-term exceeds 2x long-term average)
	PanicThresholdLabel = AutoscalingLabelPrefix + "panic-threshold"

	// ScaleUpCooldownWindowLabel sets the cooldown window for scale-up decisions
	// Value: duration (e.g., "0s", "60s")
	ScaleUpCooldownWindowLabel = AutoscalingLabelPrefix + "scale-up-cooldown-window"

	// ScaleDownCooldownWindowLabel sets the cooldown window for scale-down decisions
	// Value: duration (e.g., "300s", "5m")
	ScaleDownCooldownWindowLabel = AutoscalingLabelPrefix + "scale-down-cooldown-window"

	// ScaleToZeroLabel enables/disables scaling to zero replicas
	// Value: bool (e.g., "true", "false")
	ScaleToZeroLabel = AutoscalingLabelPrefix + "scale-to-zero"
)
