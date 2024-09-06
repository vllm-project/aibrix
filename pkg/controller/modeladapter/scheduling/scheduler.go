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

package scheduling

import (
	"context"
	"errors"

	v1 "k8s.io/api/core/v1"

	"github.com/aibrix/aibrix/pkg/cache"
)

type Scheduler interface {
	// SelectPod returns the pod to schedule model adapter
	SelectPod(ctx context.Context, pods []v1.Pod) (*v1.Pod, error)
}

// NewScheduler leverages the factory method to choose the right scheduler
func NewScheduler(policyName string, c *cache.Cache) (Scheduler, error) {
	switch policyName {
	case "leastAdapters":
		return NewLeastAdapters(c), nil
	default:
		return nil, errors.New("unknown scheduler policy")
	}
}
