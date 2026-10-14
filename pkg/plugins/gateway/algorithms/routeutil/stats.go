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

package routeutil

import "math"

// Mean calculates the mean of a slice of float64 numbers.
func Mean(numbers []float64) float64 {
	if len(numbers) == 0 {
		return 0
	}
	sum := 0.0
	for _, number := range numbers {
		sum += number
	}
	return sum / float64(len(numbers))
}

// StandardDeviation calculates the standard deviation of a slice of float64 numbers.
func StandardDeviation(numbers []float64) float64 {
	if len(numbers) <= 1 {
		return 0
	}
	avg := Mean(numbers)
	sumOfSquares := 0.0
	for _, number := range numbers {
		sumOfSquares += (number - avg) * (number - avg)
	}
	variance := sumOfSquares / float64(len(numbers)-1)
	return math.Sqrt(variance)
}
