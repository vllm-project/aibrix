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

package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

// prefillCandidates scores one prefill pod per roleset with the given in-flight
// counts at the shipped sigma of 2 and returns the names of the pods kept.
func prefillCandidates(t *testing.T, counts []int) map[string]bool {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "hello world", "req-load-cap", "user")
	t.Cleanup(ctx.Delete)
	ctx.SetRoutingOverrides(&types.RoutingOverrides{PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 2}})

	tracker := pd.NewPrefillRequestTracker()
	pods := make([]*v1.Pod, 0, len(counts))
	for i, cnt := range counts {
		name := fmt.Sprintf("prefill-%d", i)
		pods = append(pods, makePDPod(name, fmt.Sprintf("roleset-%d", i), "", nil))
		addRequests(tracker, name, cnt)
	}
	r := &pdRouter{
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefillRequestTracker: tracker,
	}
	scores, _, _ := r.scorePrefillPods(ctx, pods, nil)
	kept := map[string]bool{}
	for _, score := range scores {
		kept[score.Pod.Name] = true
	}
	return kept
}

func TestScorePrefillPods_LoadCapSkipsHotPodInSmallPool(t *testing.T) {
	// [12,0,0,0]: mean 3, sample stddev 6, so the stddev threshold is 15 and can
	// never be reached by 4 pods. The cap is 1.5×(3+1) = 6.
	kept := prefillCandidates(t, []int{12, 0, 0, 0})
	assert.False(t, kept["prefill-0"], "a pod far above the others is skipped")
	assert.Len(t, kept, 3)

	// Two pods, where the stddev filter can never fire: [6,0] has cap 6 and keeps
	// both, [7,0] has cap 6.75 and skips the busy pod.
	assert.Len(t, prefillCandidates(t, []int{6, 0}), 2, "a lead at the cap is not skipped")
	kept = prefillCandidates(t, []int{7, 0})
	assert.False(t, kept["prefill-0"], "the cap works for a two-pod pool")
	assert.Len(t, kept, 1)
}

func TestScorePrefillPods_LoadCapKeepsIdleAndEvenPools(t *testing.T) {
	assert.Len(t, prefillCandidates(t, []int{0, 0, 0, 0}), 4, "an idle pool keeps every pod")
	assert.Len(t, prefillCandidates(t, []int{1, 0, 0, 0}), 4, "the +1 keeps a single in-flight prefill from tripping the cap")
	assert.Len(t, prefillCandidates(t, []int{6, 2, 2, 2}), 4, "a moderate lead stays within the cap of 6")
	assert.Len(t, prefillCandidates(t, []int{6, 6, 6, 6}), 4, "an evenly loaded pool keeps every pod")
}

func TestScorePrefillPods_LoadCapFactorIsConfigurable(t *testing.T) {
	defer func(f float64) { aibrixPrefillLoadCapFactor = f }(aibrixPrefillLoadCapFactor)

	aibrixPrefillLoadCapFactor = 4 // cap 4×(3+1) = 16
	assert.Len(t, prefillCandidates(t, []int{12, 0, 0, 0}), 4, "a looser cap keeps the busy pod")

	aibrixPrefillLoadCapFactor = 1.25 // cap 1.25×(3+1) = 5
	kept := prefillCandidates(t, []int{6, 2, 2, 2})
	assert.False(t, kept["prefill-0"], "a tighter cap skips a smaller lead")
	assert.Len(t, kept, 3)
}

// The factor is read from the environment once at package init, so the wiring is
// checked in a child test binary started with the variable set.
func TestPrefillLoadCapFactorReadFromEnv(t *testing.T) {
	if os.Getenv("AIBRIX_TEST_PREFILL_LOAD_CAP_CHILD") == "1" {
		assert.Equal(t, 3.0, aibrixPrefillLoadCapFactor)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrefillLoadCapFactorReadFromEnv$")
	cmd.Env = append(os.Environ(),
		"AIBRIX_TEST_PREFILL_LOAD_CAP_CHILD=1",
		"AIBRIX_PREFILL_LOAD_CAP_FACTOR=3",
	)
	out, err := cmd.CombinedOutput()
	assert.NoError(t, err, "child test failed:\n%s", out)
}
