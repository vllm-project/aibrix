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

package prefill

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

// TestMain installs the process defaults a request without overrides falls
// back to: the environment-derived PD knobs, plus the prefill request timeout
// the routing algorithm package adds when it assembles the table at startup.
func TestMain(m *testing.M) {
	defaults := pd.EnvOverrides()
	defaults.PrefillRequestTimeout = 30 * time.Second
	types.SetDefaultPDOverrides(&defaults)
	os.Exit(m.Run())
}

func TestEffectiveRequestTimeoutFromProfile(t *testing.T) {
	restore := types.DefaultPDOverrides()
	defaults := *restore
	defaults.PrefillRequestTimeout = 30 * time.Second
	types.SetDefaultPDOverrides(&defaults)
	t.Cleanup(func() { types.SetDefaultPDOverrides(restore) })

	exec := NewDefaultExecutor(&http.Client{}, pd.NewPrefillRequestTracker()).(*DefaultExecutor)
	assert.Equal(t, 30*time.Second, exec.effectiveRequestTimeout(&types.RoutingContext{}),
		"a request without a leg keeps the process default")

	ctx := types.NewRoutingContext(context.Background(), "pd", "m", "msg", "req-timeout", "user")
	defer ctx.Delete()
	overrides := *types.DefaultPDOverrides()
	overrides.PrefillRequestTimeout = 90 * time.Second
	ctx.SetPDOverrides(&overrides)
	assert.Equal(t, 90*time.Second, exec.effectiveRequestTimeout(ctx))

	plain := types.NewRoutingContext(context.Background(), "pd", "m", "msg", "req-plain", "user")
	defer plain.Delete()
	assert.Equal(t, 30*time.Second, exec.effectiveRequestTimeout(plain))
}
