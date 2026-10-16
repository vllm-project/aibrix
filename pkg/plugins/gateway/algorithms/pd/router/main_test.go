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
	"os"
	"testing"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/types"
)

// TestMain installs the PD and prefix-cache halves of the process default table, the way the
// routing algorithm package does at startup, so a request without a profile reads the
// environment-derived knobs instead of zeros. The PD prefill scorer reads the prefix-cache
// standard-deviation factor, which is why the prefix-cache half is needed too.
func TestMain(m *testing.M) {
	types.SetDefaultRoutingOverrides(&types.RoutingOverrides{
		PrefixCache: prefixcache.PrefixCacheEnvOverrides(),
		PD:          EnvOverrides(),
	})
	os.Exit(m.Run())
}
