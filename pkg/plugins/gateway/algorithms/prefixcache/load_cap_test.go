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

package prefixcache

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hotPodChosen routes among pods with the given running counts, where pod-0 has
// the best prefix match and every other pod a weaker one, at the shipped sigma
// of 2. It reports whether pod-0 was chosen.
func hotPodChosen(t *testing.T, counts []int) bool {
	t.Helper()
	var pods []*v1.Pod
	podRequestCount := map[string]int{}
	matchedPods := map[string]int{}
	for i, c := range counts {
		name := fmt.Sprintf("pod-%d", i)
		pods = append(pods, &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		podRequestCount["default/"+name] = c
		matchedPods["default/"+name] = 50
	}
	matchedPods["default/pod-0"] = 100
	pod := getTargetPodFromMatchedPodsFromCounts(podRequestCount, pods, matchedPods, 2)
	require.NotNil(t, pod, "the least-loaded matched pod is never skipped")
	return pod.Name == "pod-0"
}

func TestLoadCapSkipsHotMatchedPodInSmallPool(t *testing.T) {
	// The stddev check cannot skip any of n ≤ 5 pods at sigma 2, so before the cap
	// the best-matched pod was chosen however busy it was.
	assert.False(t, hotPodChosen(t, []int{12, 0, 0, 0}), "cap 1.5×(3+1) = 6")
	assert.False(t, hotPodChosen(t, []int{20, 1, 1, 1, 1}), "cap 1.5×(4.8+1) = 8.7")
	assert.False(t, hotPodChosen(t, []int{7, 0}), "cap 1.5×(3.5+1) = 6.75")
}

func TestLoadCapKeepsPodsWithinIt(t *testing.T) {
	assert.True(t, hotPodChosen(t, []int{6, 0}), "6 is at the cap 1.5×(3+1)")
	assert.True(t, hotPodChosen(t, []int{0, 0, 0, 0}), "an idle pool keeps the best match")
	assert.True(t, hotPodChosen(t, []int{1, 0, 0, 0}), "one request is within the +1")
	assert.True(t, hotPodChosen(t, []int{6, 2, 2, 2}), "cap 1.5×(3+1) = 6")
	assert.True(t, hotPodChosen(t, []int{6, 6, 6, 6}), "an evenly loaded pool keeps the best match")
}

func TestLoadCapFactorIsConfigurable(t *testing.T) {
	defer func(f float64) { loadCapFactor = f }(loadCapFactor)

	loadCapFactor = 4 // cap 4×(3+1) = 16
	assert.True(t, hotPodChosen(t, []int{12, 0, 0, 0}), "a looser cap keeps the busy pod")

	loadCapFactor = 1.25 // cap 1.25×(3+1) = 5
	assert.False(t, hotPodChosen(t, []int{6, 2, 2, 2}), "a tighter cap skips a smaller lead")
}

// The factor is read from the environment once at package init, so the wiring is
// checked in a child test binary started with the variable set.
func TestLoadCapFactorReadFromEnv(t *testing.T) {
	if os.Getenv("AIBRIX_TEST_PREFIX_CACHE_LOAD_CAP_CHILD") == "1" {
		assert.Equal(t, 3.0, loadCapFactor)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoadCapFactorReadFromEnv$")
	cmd.Env = append(os.Environ(),
		"AIBRIX_TEST_PREFIX_CACHE_LOAD_CAP_CHILD=1",
		"AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR=3",
	)
	out, err := cmd.CombinedOutput()
	assert.NoError(t, err, "child test failed:\n%s", out)
}
