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

package queue

// queueOptions carries the SLOQueue policy switches. The defaults below are the
// shipped behavior; the switches are struct fields rather than constants so
// tests can pin every branch of the policy and, later, configuration can feed
// them without touching the queue internals.
//
// Switch matrix:
//
//	fifoOnNonSLOViolation: when true, candidates that both rank below zero are
//	  served in arrival order instead of rank order.
//	queueOverallSLO: when true, ranking accounts for the requests already queued
//	  on the head's subqueue (queueRank) instead of ranking the head alone (rank).
//	monogenousGPURouting: when true, a candidate is routed per deployment
//	  profile, most relaxing first; when false, it is routed against the whole
//	  pod set in one call.
//	monogenousGPURoutingOnly: when true, only the most relaxing profile is
//	  tried. It is only meaningful while monogenousGPURouting is enabled;
//	  normalized() enforces that implication.
type queueOptions struct {
	fifoOnNonSLOViolation    bool
	queueOverallSLO          bool
	monogenousGPURouting     bool
	monogenousGPURoutingOnly bool
}

const (
	fifoOnNonSLOViolationDefault    = false
	queueOverallSLODefault          = false
	monogenousGPURoutingDefault     = true
	monogenousGPURoutingOnlyDefault = false
)

// defaultQueueOptions returns the shipped policy switches.
func defaultQueueOptions() queueOptions {
	return queueOptions{
		fifoOnNonSLOViolation:    fifoOnNonSLOViolationDefault,
		queueOverallSLO:          queueOverallSLODefault,
		monogenousGPURouting:     monogenousGPURoutingDefault,
		monogenousGPURoutingOnly: monogenousGPURoutingOnlyDefault,
	}
}

// normalized enforces the single constraint between the switches: trying only
// the most relaxing profile is a refinement of monogenous GPU routing, so it
// cannot stand on its own.
func (o queueOptions) normalized() queueOptions {
	if o.monogenousGPURoutingOnly {
		o.monogenousGPURouting = true
	}
	return o
}
