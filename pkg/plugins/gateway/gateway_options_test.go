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

package gateway

import "testing"

func TestNewServerWithOptionsUsesInjectedCache(t *testing.T) {
	injected := &MockCache{}

	server := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: injected})
	if server.cache != injected {
		t.Fatalf("NewServerWithOptions() cache = %p, want injected cache %p", server.cache, injected)
	}
	if server.routerManager == nil {
		t.Fatal("NewServerWithOptions() with Cache-only options must create a local router manager")
	}
}
