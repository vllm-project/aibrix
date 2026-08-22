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

package pametrics

import (
	"errors"
	"math"
	"strconv"
)

var (
	errInvalidTargetValue     = errors.New("must be a valid number")
	errNonPositiveTargetValue = errors.New("must be a finite number greater than 0")
)

// ParseTargetValue parses the plain positive floating-point format shared by
// PodAutoscaler admission, controller validation, and HPA generation.
func ParseTargetValue(value string) (float64, error) {
	targetValue, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, errInvalidTargetValue
	}
	if math.IsNaN(targetValue) || math.IsInf(targetValue, 0) || targetValue <= 0 {
		return 0, errNonPositiveTargetValue
	}
	return targetValue, nil
}
