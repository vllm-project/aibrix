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

package tokenizer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVLLMAdapterPrepareChatTokenizeRequestTools(t *testing.T) {
	adapter := newVLLMAdapter("m")
	messages := []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}

	t.Run("tools forwarded verbatim", func(t *testing.T) {
		tools := json.RawMessage(`[{"type":"function","function":{"name":"f","description":"a < b"}}]`)
		req, err := adapter.PrepareTokenizeRequest(TokenizeInput{Type: ChatInput, Messages: messages, Tools: tools})
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)

		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &got))
		assert.JSONEq(t, string(tools), string(got["tools"]))
	})

	t.Run("no tools key without tools", func(t *testing.T) {
		req, err := adapter.PrepareTokenizeRequest(TokenizeInput{Type: ChatInput, Messages: messages})
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)

		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &got))
		assert.NotContains(t, got, "tools")
	})
}
