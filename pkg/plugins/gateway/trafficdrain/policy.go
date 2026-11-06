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

// Package trafficdrain picks the pods the gateway would drain before a model scales in
// (RFC #3187). The gateway and the autoscaler do not talk to each other: the autoscaler only
// lowers a replica count and the workload controller picks the pod to delete, so the gateway
// has to drain the pod that the controller will delete. A Policy makes that choice from the
// pods the gateway already watches, and never writes to the cluster.
//
// Nothing in the gateway calls these policies yet. The drain loop and the routing change
// come in later steps, off by default.
package trafficdrain

import (
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
)

// SkipReason says why a policy picked no pods for a round.
type SkipReason string

const (
	// SkipOwners means the model's active pods are not all owned by one ReplicaSet: a rollout
	// is under way, several Deployments serve the model, or the workload is not a Deployment.
	SkipOwners SkipReason = "owners"
	// SkipTooFewPods means there are fewer than two routable pods, or the scale-in would remove
	// every active pod or every routable one.
	SkipTooFewPods SkipReason = "too_few_pods"
	// SkipTie means the controller's choice cannot be predicted: at the cut between the pods it
	// deletes and the pods it keeps, two pods are equal for every rule it applies, and the
	// controller's sort leaves them in any order.
	SkipTie SkipReason = "tie"
	// SkipOrderChanges means the pods the controller would delete change within the horizon,
	// because pods move to a new age bucket.
	SkipOrderChanges SkipReason = "order_changes"
	// SkipDraining means a predicted pod already carries the aibrix.ai/draining annotation.
	SkipDraining SkipReason = "draining"
)

// Request is the input of one policy decision.
type Request struct {
	// Pods are the model's pods as the gateway's pod cache holds them, in any state.
	Pods []*v1.Pod
	// K is the number of pods the scale-in is expected to remove.
	K int
	// Now is the time of the decision.
	Now time.Time
	// Horizon is how long a drain that starts now may last. The answer must hold from Now
	// until Now+Horizon, since the controller may scale in at any time in between.
	Horizon time.Duration
}

// Result is the output of one policy decision.
type Result struct {
	// Next are the K pods expected to be removed next, in the order the policy expects.
	Next []*v1.Pod
	// Drain are the pods in Next that are routable and so need draining. A pod that is
	// unscheduled, pending or not ready is removed first and gets no traffic to drain.
	Drain []*v1.Pod
	// Skip is set when the policy picks no pods for this round; Next and Drain are empty then.
	Skip SkipReason
}

// Policy picks the pods to drain before a scale-in. Implementations only read the pods they
// are given; they must not write to the cluster.
type Policy interface {
	// Name is the value that selects the policy in a model's configuration.
	Name() string
	// Candidates returns up to req.K pods to drain, or a skip reason.
	Candidates(req Request) Result
}

var policies = map[string]Policy{}

func register(p Policy) {
	policies[p.Name()] = p
}

// Get returns the policy registered under name.
func Get(name string) (Policy, bool) {
	p, ok := policies[name]
	return p, ok
}

// Names returns the names of the registered policies, sorted.
func Names() []string {
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
