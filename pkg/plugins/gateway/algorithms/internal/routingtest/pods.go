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

// Package routingtest holds the fakes and fixtures shared by the tests of the routing
// algorithm packages (algorithms, algorithms/least, algorithms/prefixcache, ...). It is
// internal so that only those packages can depend on it.
package routingtest

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// TestModel is the model name carried by the pods TestPods returns.
	TestModel = "test-model"
	// Namespace is the namespace of the pods TestPods returns.
	Namespace = "default"
)

// NewPod constructs a Pod with basic configuration including name, IP address, ready status, and labels.
// returns a pointer to the created Pod object
func NewPod(name, ip string, ready bool, labels map[string]string) *v1.Pod {
	condition := v1.ConditionFalse
	if ready {
		condition = v1.ConditionTrue
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Status: v1.PodStatus{
			PodIP: ip,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: condition,
				},
			},
		},
	}
}

// TestPods returns ready single-port pods named names, at 10.0.0.1, 10.0.0.2, ... on port 8000.
func TestPods(names ...string) []*v1.Pod {
	pods := make([]*v1.Pod, len(names))
	for i, name := range names {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: Namespace,
				Labels: map[string]string{
					constants.ModelLabelName: TestModel,
					constants.ModelLabelPort: "8000",
				},
			},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.0.%d", i+1),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
	}
	return pods
}

// PodKey is the ListPortsForPod key of a TestPods pod.
func PodKey(name string) string { return utils.GeneratePodKey(Namespace, name) }

// MockPodList is a types.PodList backed by fixed slices.
type MockPodList struct {
	pods    []*v1.Pod
	indexes map[string][]*v1.Pod
}

func (m *MockPodList) All() []*v1.Pod {
	return m.pods
}

func (m *MockPodList) Len() int {
	return len(m.pods)
}

func (m *MockPodList) Indexes() []string {
	keys := make([]string, 0, len(m.indexes))
	for k := range m.indexes {
		keys = append(keys, k)
	}
	return keys
}

func (m *MockPodList) ListByIndex(index string) []*v1.Pod {
	if pods, ok := m.indexes[index]; ok {
		return pods
	}
	return nil
}

func (m *MockPodList) ListPortsForPod() map[string][]int {
	return nil
}

// NewMockPodList returns a MockPodList over pods; a nil indexes means no indexes.
func NewMockPodList(pods []*v1.Pod, indexes map[string][]*v1.Pod) *MockPodList {
	if indexes == nil {
		indexes = make(map[string][]*v1.Pod)
	}
	return &MockPodList{
		pods:    pods,
		indexes: indexes,
	}
}

// PodListWithPorts is a MockPodList that also reports per-pod ports, the way utils.PodArray does for
// pods carrying the model.aibrix.ai/port label.
type PodListWithPorts struct {
	*MockPodList
	ports map[string][]int
}

func (l *PodListWithPorts) ListPortsForPod() map[string][]int { return l.ports }

// NewPodListWithPorts returns a PodListWithPorts over pods, with ports keyed by pod key.
func NewPodListWithPorts(pods []*v1.Pod, ports map[string][]int) *PodListWithPorts {
	return &PodListWithPorts{MockPodList: NewMockPodList(pods, nil), ports: ports}
}

// PodsFromCache returns every pod of c as a types.PodList.
func PodsFromCache(c *cache.Store) *utils.PodArray {
	return &utils.PodArray{Pods: c.ListPods()}
}

// SameNamedPod returns a ready pod named "worker-0" in namespace, the shape a
// StatefulSet or LeaderWorkerSet produces in each namespace it is deployed to.
func SameNamedPod(namespace, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: namespace},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// DefaultNamespaceKeys rekeys a pod-name map by pod key (namespace/name) in the
// "default" namespace, the form the per-request maps and the prefix index use.
func DefaultNamespaceKeys(byName map[string]int) map[string]int {
	byKey := make(map[string]int, len(byName))
	for name, v := range byName {
		byKey["default/"+name] = v
	}
	return byKey
}

// ReadyPods returns four ready pods, p1..p4 at 1.1.1.1..4.4.4.4, of one deployment.
func ReadyPods() []*v1.Pod {
	return []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p3", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "3.3.3.3",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "p4", Labels: map[string]string{
				utils.DeploymentIdentifier: "deployment",
			}},
			Status: v1.PodStatus{
				PodIP: "4.4.4.4",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
	}
}
