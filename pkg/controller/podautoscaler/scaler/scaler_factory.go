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

package scaler

import (
	"fmt"
	"time"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
)

// NewAutoscalerFactory creates an Autoscaler based on the given ScalingStrategy
func NewAutoscalerFactory(strategy autoscalingv1alpha1.ScalingStrategyType) (Scaler, error) {
	// after update, the XpaAutoscaler must be associated with an instantiated PA, rather than an empty scaler that awaits filling.
	// But NewAutoscalerFactory doesn't be used, so we temporarily pass into nil
	switch strategy {
	case autoscalingv1alpha1.KPA:
		autoscaler, err := NewKpaAutoscaler(0, nil, time.Now())
		if err != nil {
			return nil, err
		}
		return autoscaler, nil
	case autoscalingv1alpha1.APA:
		autoscaler, err := NewApaAutoscaler(0, nil)
		if err != nil {
			return nil, err
		}
		return autoscaler, nil
	default:
		return nil, fmt.Errorf("unsupported scaling strategy: %s", strategy)
	}
}
