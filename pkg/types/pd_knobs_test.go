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

package types

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPDOverridesRoundTrip(t *testing.T) {
	installDefaultOverrides(t, &RoutingOverrides{PD: PDOverrides{MinMatchPct: 10}})

	ctx := NewRoutingContext(context.Background(), "pd", "model", "message", "req-overrides", "user")
	assert.Equal(t, 10.0, ctx.PDOverrides().MinMatchPct, "a request without a profile reads the process defaults")

	overrides := &PDOverrides{MinMatchPct: 42, Abort: PDAbortOverrides{Timeout: 3 * time.Second}}
	ctx.SetPDOverrides(overrides)
	assert.Same(t, overrides, ctx.PDOverrides())
	ctx.Delete()

	next := NewRoutingContext(context.Background(), "pd", "model", "message", "req-next", "user")
	defer next.Delete()
	assert.Equal(t, 10.0, next.PDOverrides().MinMatchPct, "reset installs a fresh leg, so overrides never leak between requests")

	var leg *PDLegState
	assert.Equal(t, 10.0, leg.PDOverrides().MinMatchPct, "a nil leg reads the process defaults")
	leg.SetPDOverrides(overrides) // nil receiver: must not panic

	var nilCtx *RoutingContext
	assert.Equal(t, 10.0, nilCtx.PDOverrides().MinMatchPct)
	nilCtx.SetPDOverrides(overrides) // nil receiver: must not panic
}

func TestPDOverridesAreScopedToOneIncarnation(t *testing.T) {
	installDefaultOverrides(t, &RoutingOverrides{})

	ctx := NewRoutingContext(context.Background(), "pd", "model", "message", "req-scope", "user")
	ctx.SetPDOverrides(&PDOverrides{MinMatchPct: 25})
	assert.Equal(t, 25.0, ctx.PDOverrides().MinMatchPct)

	leg := ctx.PDLeg()
	ctx.Delete()
	// The leg outlives the pooled context: the async prefill and abort paths
	// read the incarnation's values off it after the context is recycled.
	assert.Equal(t, 25.0, leg.PDOverrides().MinMatchPct)
}
