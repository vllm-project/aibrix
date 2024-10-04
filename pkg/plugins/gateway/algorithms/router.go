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

	v1 "k8s.io/api/core/v1"
)

const (
	num_requests_running  = "num_requests_running"
	num_requests_waiting  = "num_requests_waiting"
	num_requests_swapped  = "num_requests_swapped"
	throughput_prompt     = "avg_prompt_throughput_toks_per_s"
	throughput_generation = "avg_generation_throughput_toks_per_s"
	latency               = "e2e_request_latency_seconds_sum"

	podPort = 8000
)

type Router interface {
	// Returns the target pod
	Route(ctx context.Context, pods map[string]*v1.Pod) (string, error)
}
