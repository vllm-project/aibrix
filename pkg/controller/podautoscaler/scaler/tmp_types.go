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

package kpa

import (
	"sync"
	"time"
)

// DeciderSpec defines parameters for scaling decisions.
type DeciderSpec struct {
	MaxScaleUpRate   float64       // Maximum rate at which to scale up
	MaxScaleDownRate float64       // Maximum rate at which to scale down
	TargetValue      float64       // Target metric value per pod
	TotalValue       float64       // Total metric capacity per pod
	PanicThreshold   float64       // Threshold to enter panic mode
	StableWindow     time.Duration // Duration to stabilize before exiting panic mode
	ScaleDownDelay   time.Duration // Delay before applying scale-down
	ActivationScale  int32         // Minimum scale at which to activate the service
}

// ScaleResult contains the results of a scaling decision.
type ScaleResult struct {
	DesiredPodCount     int32 // Suggested number of pods
	ExcessBurstCapacity int32 // Calculated excess capacity
	ScaleValid          bool  // Validity of the scale result
}

// Autoscaler represents an instance of the autoscaling engine.
type Autoscaler struct {
	specMux      sync.RWMutex
	deciderSpec  *DeciderSpec
	panicTime    time.Time
	maxPanicPods int32
	delayWindow  *timeWindow
}

// timeWindow is used to manage delay in scale-down decisions.
type timeWindow struct {
	lastRecorded time.Time
	count        int32
}

// record updates the count in the time window.
func (tw *timeWindow) record(currentTime time.Time, count int32) {
	tw.lastRecorded = currentTime
	tw.count = count
}

// current returns the current count in the time window.
func (tw *timeWindow) current() int32 {
	return tw.count
}

// NewAutoscaler creates a new instance of the autoscaler.
func NewAutoscaler(spec *DeciderSpec) *Autoscaler {
	return &Autoscaler{
		deciderSpec: spec,
		delayWindow: &timeWindow{},
	}
}
