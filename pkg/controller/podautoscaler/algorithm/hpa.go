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

package algorithm

import (
	"context"
)

// HPAAlgorithm is a placeholder for HPA strategy (actual HPA is handled by K8s HPA resources)
// This is a stateless struct that can be safely reused across goroutines
type HPAAlgorithm struct{}

var _ ScalingAlgorithm = (*HPAAlgorithm)(nil)

// ComputeRecommendation for HPA just returns current replicas as HPA is managed by K8s
func (a *HPAAlgorithm) ComputeRecommendation(ctx context.Context, request ScalingRequest) (*ScalingRecommendation, error) {
	// HPA scaling is handled by Kubernetes HPA controller
	// This is just a placeholder that maintains current state
	return &ScalingRecommendation{
		DesiredReplicas: request.CurrentReplicas,
		Confidence:      1.0,
		Reason:          "HPA managed by Kubernetes",
		Algorithm:       "hpa",
		ScaleValid:      true,
		Metadata:        map[string]interface{}{},
	}, nil
}

// GetAlgorithmType returns the algorithm type
func (a *HPAAlgorithm) GetAlgorithmType() string {
	return "hpa"
}
