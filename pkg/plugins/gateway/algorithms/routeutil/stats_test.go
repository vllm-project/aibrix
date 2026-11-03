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

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadCap(t *testing.T) {
	assert.Equal(t, 6.0, LoadCap(3, 1.5))
	assert.Equal(t, 1.5, LoadCap(0, 1.5), "an idle pool's cap is the factor")
	// A factor below 1 is raised to 1, so the cap stays above the mean and the
	// least-loaded pod can never be skipped.
	assert.Equal(t, 4.0, LoadCap(3, 0.5))
	assert.True(t, math.IsInf(LoadCap(3, math.NaN()), 1), "a NaN factor disables the cap")
}
