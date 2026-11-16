/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"os"
	"testing"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/blacklist"
)

// TestMain turns the pod-blacklist master switch on for the gateway package's
// behavioural tests. Production default is off; tests that cover the disabled
// path flip blacklist.Enabled for their own duration.
func TestMain(m *testing.M) {
	blacklist.Enabled = true
	os.Exit(m.Run())
}
