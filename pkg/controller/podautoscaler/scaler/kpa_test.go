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

package scaler

import (
	"log"
	"testing"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/aggregation"
)

func TestScale(t *testing.T) {
	kpaScaler, err := NewKpaAutoscaler(5,
		&DeciderSpec{
			MaxScaleUpRate:   1.5,
			MaxScaleDownRate: 0.75,
			TargetValue:      100,
			TotalValue:       500,
			PanicThreshold:   2.0,
			StableWindow:     1 * time.Minute,
			ScaleDownDelay:   1 * time.Minute,
			ActivationScale:  2,
		},
		time.Time{}, 10, aggregation.NewTimeWindow(30*time.Second, 1*time.Second),
	)
	if err != nil {
		t.Errorf("Failed to create KpaAutoscaler: %v", err)
	}

	observedStableValue := 120.0
	observedPanicValue := 240.0
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		log.Printf("Scaling evaluation at %s", now)
		result := kpaScaler.Scale(observedStableValue, observedPanicValue, now)
		log.Printf("Scale result: Desired Pod Count = %d, Excess Burst Capacity = %d, Valid = %v", result.DesiredPodCount, result.ExcessBurstCapacity, result.ScaleValid)

		// Stop if the desired pod count has increased
		if result.DesiredPodCount > 5 {
			log.Println("Scaling up, breaking loop.")
			break
		}
	}
}
