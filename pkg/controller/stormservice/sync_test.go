/*
Copyright 2025 The Aibrix Team.

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

package stormservice

import "testing"

func TestCalculateReplicas(t *testing.T) {
	type args struct {
		currentReplicas int32
		updatedReplicas int32
		desiredReplicas int32
		desiredCurrent  int32
		desiredUpdated  int32
	}
	tests := []args{
		{
			currentReplicas: 1,
			updatedReplicas: 1,
			desiredReplicas: 4,
			desiredCurrent:  2,
			desiredUpdated:  2,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 1,
			desiredReplicas: 9,
			desiredCurrent:  6,
			desiredUpdated:  3,
		},
		{
			currentReplicas: 6,
			updatedReplicas: 3,
			desiredReplicas: 3,
			desiredCurrent:  2,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 10,
			desiredReplicas: 10,
			desiredCurrent:  1,
			desiredUpdated:  9,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 20,
			desiredReplicas: 25,
			desiredCurrent:  1,
			desiredUpdated:  24,
		},
		{
			currentReplicas: 1,
			updatedReplicas: 20,
			desiredReplicas: 100,
			desiredCurrent:  5,
			desiredUpdated:  95,
		},
		{
			currentReplicas: 10,
			updatedReplicas: 1,
			desiredReplicas: 10,
			desiredCurrent:  9,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 2,
			desiredReplicas: 5,
			desiredCurrent:  3,
			desiredUpdated:  2,
		},
		{
			currentReplicas: 5,
			updatedReplicas: 5,
			desiredReplicas: 3,
			desiredCurrent:  2,
			desiredUpdated:  1,
		},
		{
			currentReplicas: 0,
			updatedReplicas: 0,
			desiredReplicas: 5,
			desiredCurrent:  0,
			desiredUpdated:  5,
		},
		{
			currentReplicas: 2,
			updatedReplicas: 3,
			desiredReplicas: 0,
			desiredCurrent:  0,
			desiredUpdated:  0,
		},
	}
	for _, test := range tests {
		c, u := calculateReplicas(test.desiredReplicas, test.currentReplicas, test.updatedReplicas)
		if c != test.desiredCurrent || u != test.desiredUpdated {
			t.Errorf("failed %+v, current %d, updated %d", test, c, u)
		}
	}
}
