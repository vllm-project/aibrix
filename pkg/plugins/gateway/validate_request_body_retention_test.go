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

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// liveHeapBytes returns the heap still reachable after a full collection.
func liveHeapBytes() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// TestValidateRequestBody_ModelDoesNotPinRequestBody guards a production memory
// leak. sonic.Unmarshal copies the whole request body into a private string and,
// with its default config, decodes strings as slices of that copy. The model name
// outlives the request (routing context, in-flight gauges, log fields), so if it
// still pointed into the copy, every request that left its model behind would pin
// a full request body -- hundreds of KB for agent traffic.
//
// The body carries a large unknown field so the only thing that can keep the
// copy reachable is the returned model.
func TestValidateRequestBody_ModelDoesNotPinRequestBody(t *testing.T) {
	const paddingBytes = 8 << 20
	padding := strings.Repeat("x", paddingBytes)

	tests := []struct {
		name string
		path string
		body string
	}{
		{"responses", PathResponses, `{"model":"m","input":"hi","padding":"` + padding + `"}`},
		{"chat completions", PathChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}],"padding":"` + padding + `"}`},
		{"messages", PathMessages, `{"model":"m","messages":[{"role":"user","content":"hi"}],"padding":"` + padding + `"}`},
		{"completions", PathCompletions, `{"model":"m","prompt":"hi","padding":"` + padding + `"}`},
		{"embeddings", PathEmbeddings, `{"model":"m","input":"hi","padding":"` + padding + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			before := liveHeapBytes()

			model, _, _, _, errRes := validateRequestBody("test-request-id", tt.path, body, utils.User{})
			require.Nil(t, errRes)
			require.Equal(t, "m", model)

			// Holding model must not hold the decoder's copy of the body.
			retained := liveHeapBytes() - before
			runtime.KeepAlive(model)
			runtime.KeepAlive(body)
			require.Less(t, retained, int64(paddingBytes/4),
				"model keeps %d bytes of the %d-byte request body reachable", retained, paddingBytes)
		})
	}
}
