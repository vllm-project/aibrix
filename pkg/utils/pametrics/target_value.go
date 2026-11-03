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
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	errInvalidTargetValue     = errors.New("must be a valid number")
	errNonPositiveTargetValue = errors.New("must be a finite number greater than 0")
	errTargetValueOutOfRange  = errors.New("must be representable as an HPA metric target")
)

const bytesPerMiB = int64(1024 * 1024)

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

// ParseHPATargetValue parses a target value and verifies that rounding it up
// cannot overflow the integer representation used by the HPA API.
func ParseHPATargetValue(value, targetMetric string) (float64, error) {
	targetValue, err := ParseTargetValue(value)
	if err != nil {
		return 0, err
	}

	roundedTarget := math.Ceil(targetValue)
	var inRange bool
	switch strings.ToLower(targetMetric) {
	case "cpu":
		inRange = roundedTarget <= math.MaxInt32
	case "memory":
		inRange = roundedTarget <= float64(math.MaxInt64/bytesPerMiB)
	default:
		// float64(math.MaxInt64) rounds to 2^63, which is already outside
		// the int64 range, so use an exclusive upper bound here.
		inRange = roundedTarget < math.Ldexp(1, 63)
	}
	if !inRange {
		return 0, fmt.Errorf("%w for targetMetric %q", errTargetValueOutOfRange, targetMetric)
	}
	return targetValue, nil
}
