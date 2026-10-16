/*
Copyright 2024 The Aibrix Team.

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

// A pooled context handed to the next request must not carry the previous request's composite:
// Select only sets ResolvedStrategy on success.
func TestResolvedStrategyClearedOnReuse(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "least-kv-cache", "model", "msg", "r1", "")
	assert.Empty(t, ctx.ResolvedStrategy, "empty until Select runs")
	ctx.ResolvedStrategy = "least-kv-cache,load-balance:1"

	ctx.reset(context.Background(), "random", "model", "msg", "r2", "")
	assert.Empty(t, ctx.ResolvedStrategy)
	assert.Equal(t, RoutingAlgorithm("random"), ctx.Algorithm)
}
