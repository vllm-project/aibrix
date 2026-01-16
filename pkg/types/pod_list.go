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
package types

import v1 "k8s.io/api/core/v1"

// PodList is an interface for a list of pods and support for indexing and querying pods by index.
type PodList interface {
	// Len returns the number of pods in the list.
	Len() int

	// All returns a slice of all pods in the list.
	All() []*v1.Pod

	// Indexes returns a slice of indexes for querying pods by index.
	Indexes() []string

	// ListByIndex returns a slice of pods that match the given index.
	ListByIndex(index string) []*v1.Pod

	// ListPortsForPod returns a map of portList that bind with pod, key podname
	ListPortsForPod() map[string][]int

	// HasMultiPortPods returns true if pod with multiPorts or false if pod with single port
	HasMultiPortPods() bool
}
