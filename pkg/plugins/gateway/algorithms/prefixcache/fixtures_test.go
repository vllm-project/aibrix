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

package prefixcache

import "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/internal/routingtest"

// The fixtures live in internal/routingtest so the tests of the other routing packages can use
// them too. These aliases keep the test bodies in this package unchanged.
var (
	newPod               = routingtest.NewPod
	newMockPodList       = routingtest.NewMockPodList
	podsFromCache        = routingtest.PodsFromCache
	sameNamedPod         = routingtest.SameNamedPod
	defaultNamespaceKeys = routingtest.DefaultNamespaceKeys
	getReadyPods         = routingtest.ReadyPods
)
