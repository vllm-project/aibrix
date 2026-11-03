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

// LoadCap returns the bounded-load cap factor×(mean+1) on a pod's in-flight
// request count. A stddev filter alone cannot skip any pod when the pool is
// small: no value in n samples sits more than (n-1)/√n sample standard
// deviations above their mean, which is below 2 for n ≤ 5. The busiest of n
// pods holds at most n×mean, so the cap can fire whenever the factor is below
// n; a factor of 1.5 reaches two-pod pools. The +1 keeps an idle pool from
// rejecting its first request, and a factor below 1 is raised to 1 so the cap
// stays above the mean and the least-loaded pod is never skipped.
//
// A NaN factor disables the cap, so the two callers, which compare in opposite
// directions, agree on it.
func LoadCap(mean, factor float64) float64 {
	if math.IsNaN(factor) {
		return math.Inf(1)
	}
	return math.Max(factor, 1) * (mean + 1)
}
