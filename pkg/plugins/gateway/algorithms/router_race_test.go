//go:build !race

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

package routingalgorithms

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetFallback(t *testing.T) {
	deployment := "deployment"
	store := cache.NewForTest()
	store = cache.InitWithModelRouterProvider(store, NewSLORouter)
	storeCh := cache.InitWithAsyncPods(store, []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployment + "-replicaset-pod1",
				Namespace: "default",
				Labels: map[string]string{
					utils.DeploymentIdentifier: deployment,
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.0.0.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}, "test model")
	Init() // Required to initialize the router registry after store was initialized and before pods are added.
	store = <-storeCh

	_, err := store.GetRouter(RouterSLO.NewContext(context.Background(), "test model", "", "request id", ""))
	assert.Nil(t, err, "set fallback should wait router initialize")
}
