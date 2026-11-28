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

package modelclaim

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInstanceGPUCountIncludesPrefillContextParallelism(t *testing.T) {
	pm := sampleModelClaim()
	pm.Spec.EngineConfig.Args["--tensor-parallel-size"] = "2"
	pm.Spec.EngineConfig.Args["--prefill-context-parallel-size"] = "2"
	assert.Equal(t, int64(4), instanceGPUCount(pm))
}
