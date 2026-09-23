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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestEffectiveRequestTimeoutFromProfile(t *testing.T) {
	exec := NewDefaultExecutor(&http.Client{}, pd.NewPrefillRequestTracker(), 30).(*DefaultExecutor)
	assert.Equal(t, 30*time.Second, exec.effectiveRequestTimeout(&types.RoutingContext{}),
		"a request without a leg keeps the env default")

	timeoutSeconds := 90
	ctx := types.NewRoutingContext(context.Background(), "pd", "m", "msg", "req-timeout", "user")
	defer ctx.Delete()
	ctx.SetPDKnobs(&types.PDRuntimeKnobs{PrefillRequestTimeoutSeconds: &timeoutSeconds})
	assert.Equal(t, 90*time.Second, exec.effectiveRequestTimeout(ctx))

	plain := types.NewRoutingContext(context.Background(), "pd", "m", "msg", "req-plain", "user")
	defer plain.Delete()
	assert.Equal(t, 30*time.Second, exec.effectiveRequestTimeout(plain))
}
