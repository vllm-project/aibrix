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

package engine

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
)

func TestControlledFields(t *testing.T) {
	assert.Equal(t, []string{"bootstrap_host", "bootstrap_port", "bootstrap_room"}, (&SGLangHandler{}).ControlledFields())
	assert.Equal(t, []string{"disaggregated_params", "prompt", "prompt_token_ids"}, (&TRTLLMHandler{}).ControlledFields())
	assert.Equal(t, []string{"disagg_prefill_resp", "kv_transfer_params"}, (&VLLMHandler{}).ControlledFields(),
		"vLLM controls the union of every registered KV transfer agent's fields")
	assert.Empty(t, (&DefaultHandler{}).ControlledFields())
}

func TestValidateRequest(t *testing.T) {
	const okBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"extra":1,"extra":2,"max_tokens":8}`
	engines := map[string]EngineHandler{
		"vllm":    Resolve("vllm"),
		"sglang":  Resolve("sglang"),
		"trtllm":  Resolve(pd.EngineTRTLLM),
		"default": Resolve("unknown-engine"),
	}

	for name, h := range engines {
		t.Run(name+"/valid body with non-controlled duplicate", func(t *testing.T) {
			require.NoError(t, ValidateRequest([]byte(okBody), h))
		})
		for _, body := range []string{``, `{not json`, `[1,2]`, `null`, `"s"`} {
			t.Run(name+"/rejects "+body, func(t *testing.T) {
				err := ValidateRequest([]byte(body), h)
				require.Error(t, err)
				var invalid *InvalidRequestError
				assert.True(t, errors.As(err, &invalid), "must be *InvalidRequestError, got %T", err)
			})
		}
		// Common prefill control fields are rejected for every engine.
		for _, key := range pd.CommonControlledFields {
			t.Run(name+"/duplicate "+key, func(t *testing.T) {
				body := `{"model":"m","` + key + `":1,"` + key + `":2}`
				err := ValidateRequest([]byte(body), h)
				require.Error(t, err)
				var invalid *InvalidRequestError
				require.True(t, errors.As(err, &invalid))
				assert.Contains(t, err.Error(), `duplicate top-level key "`+key+`"`)
			})
		}
	}

	// Engine-specific fields are rejected only for the engines that write them.
	cases := []struct {
		key        string
		rejectedBy []string
	}{
		{"kv_transfer_params", []string{"vllm"}},
		{"disagg_prefill_resp", []string{"vllm"}},
		{"disaggregated_params", []string{"trtllm"}},
		{"prompt", []string{"trtllm"}},
		{"prompt_token_ids", []string{"trtllm"}},
		{"bootstrap_host", []string{"sglang"}},
		{"bootstrap_room", []string{"sglang"}},
	}
	for _, tc := range cases {
		body := `{"model":"m","` + tc.key + `":{"a":1},"` + tc.key + `":{"a":2}}`
		for name, h := range engines {
			rejected := false
			for _, r := range tc.rejectedBy {
				rejected = rejected || r == name
			}
			t.Run(name+"/duplicate "+tc.key, func(t *testing.T) {
				err := ValidateRequest([]byte(body), h)
				if !rejected {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), `duplicate top-level key "`+tc.key+`"`)
			})
		}
	}
}

func TestValidateSGLangRequest_MatchesValidateRequest(t *testing.T) {
	body := []byte(`{"model":"m","bootstrap_room":1,"bootstrap_room":2}`)
	err := ValidateSGLangRequest(body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SGLang request body")
	require.Error(t, ValidateRequest(body, Resolve("sglang")))
	require.NoError(t, ValidateRequest(body, Resolve("vllm")), "bootstrap_room is not controlled on vLLM")
}
