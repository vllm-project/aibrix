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

	"github.com/stretchr/testify/assert"
)

// installDefaultOverrides points the process default table at defaults for the
// duration of one test and restores an empty table afterwards.
func installDefaultOverrides(t *testing.T, defaults *RoutingOverrides) {
	t.Helper()
	SetDefaultRoutingOverrides(defaults)
	t.Cleanup(func() { SetDefaultRoutingOverrides(&RoutingOverrides{}) })
}

func TestRoutingOverridesRoundTrip(t *testing.T) {
	process := &RoutingOverrides{LoadBalance: LoadBalanceOverrides{QueuedWeight: 0.5}}
	installDefaultOverrides(t, process)

	ctx := NewRoutingContext(context.Background(), "load-balance", "model", "message", "req-overrides", "user")
	assert.Same(t, process, ctx.RoutingOverrides(), "a request without a profile reads the process defaults")

	overrides := &RoutingOverrides{LoadBalance: LoadBalanceOverrides{QueuedWeight: 0}}
	ctx.SetRoutingOverrides(overrides)
	assert.Same(t, overrides, ctx.RoutingOverrides())
	ctx.Delete()

	next := NewRoutingContext(context.Background(), "load-balance", "model", "message", "req-next", "user")
	defer next.Delete()
	assert.Same(t, process, next.RoutingOverrides(), "reset installs a fresh incarnation, so overrides never leak between requests")

	var unset *RoutingContext
	unset.SetRoutingOverrides(overrides) // nil receiver: must not panic
	assert.Same(t, process, unset.RoutingOverrides())
	unset.ClearRoutingOverrides() // nil receiver: must not panic
}

func TestDefaultPDOverridesSharesTheRoutingTable(t *testing.T) {
	installDefaultOverrides(t, &RoutingOverrides{})
	SetDefaultPDOverrides(&PDOverrides{MinMatchPct: 42})
	assert.Equal(t, 42.0, DefaultPDOverrides().MinMatchPct)
	assert.Equal(t, 42.0, DefaultRoutingOverrides().PD.MinMatchPct, "the PD defaults are one field of the table, not a second copy")

	SetDefaultPDOverrides(&PDOverrides{MinMatchPct: 7})
	assert.Equal(t, 7.0, DefaultPDOverrides().MinMatchPct)

	// Installing the whole table replaces the PD half as well: the routing
	// algorithm package passes both halves in one call.
	SetDefaultRoutingOverrides(&RoutingOverrides{})
	assert.Equal(t, 0.0, DefaultPDOverrides().MinMatchPct)
}

func TestDefaultRoutingOverridesNeverReturnsNil(t *testing.T) {
	installed := DefaultRoutingOverrides()
	SetDefaultRoutingOverrides(nil)
	assert.NotNil(t, DefaultRoutingOverrides())
	assert.NotNil(t, DefaultPDOverrides())
	SetDefaultRoutingOverrides(installed)
}
