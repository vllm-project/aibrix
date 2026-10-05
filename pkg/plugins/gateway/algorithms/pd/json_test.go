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

package pd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// nestedBody has nested objects whose key order is deliberately not sorted so
// that any map[string]any round trip would reorder them.
const nestedBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u","detail":"low"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"]}}}],"temperature":0.7,"max_tokens":128,"max_completion_tokens":128,"stream":true,"stream_options":{"include_usage":true},"min_tokens":5}`

func TestValidateJSONObject(t *testing.T) {
	assert.NoError(t, ValidateJSONObject([]byte(`{"a":1}`), "x"))
	assert.NoError(t, ValidateJSONObject([]byte(` {} `), "x"))

	for _, body := range []string{``, `{"a":`, `[1,2]`, `"s"`, `null`, `42`} {
		err := ValidateJSONObject([]byte(body), "thing")
		require.Error(t, err, "body %q", body)
		assert.Contains(t, err.Error(), "thing")
	}
}

func TestApplyPrefillControlFields(t *testing.T) {
	in := []byte(nestedBody)
	orig := append([]byte(nil), in...)

	out, err := ApplyPrefillControlFields(in, "vllm")
	require.NoError(t, err)
	assert.Equal(t, orig, in, "input must not be mutated")

	assert.Equal(t, int64(1), gjson.GetBytes(out, "max_tokens").Int())
	assert.Equal(t, int64(1), gjson.GetBytes(out, "max_completion_tokens").Int())
	assert.False(t, gjson.GetBytes(out, "stream").Bool())
	assert.False(t, gjson.GetBytes(out, "stream_options").Exists())
	assert.False(t, gjson.GetBytes(out, "min_tokens").Exists())
	assert.Equal(t, 0.7, gjson.GetBytes(out, "temperature").Float(), "client generation params must be preserved")

	for _, path := range []string{"messages", "tools"} {
		assert.Equal(t, gjson.GetBytes(in, path).Raw, gjson.GetBytes(out, path).Raw, "%s must be byte-identical", path)
	}

	// TRT-LLM: max_completion_tokens is removed instead of set.
	out, err = ApplyPrefillControlFields(in, EngineTRTLLM)
	require.NoError(t, err)
	assert.Equal(t, int64(1), gjson.GetBytes(out, "max_tokens").Int())
	assert.False(t, gjson.GetBytes(out, "max_completion_tokens").Exists())
}

func TestApplyPrefillControlFields_StableAcross100Runs(t *testing.T) {
	first, err := ApplyPrefillControlFields([]byte(nestedBody), "vllm")
	require.NoError(t, err)
	for i := 1; i < 100; i++ {
		out, err := ApplyPrefillControlFields([]byte(nestedBody), "vllm")
		require.NoError(t, err)
		require.True(t, bytes.Equal(first, out), "run %d differs:\n%s\n%s", i, first, out)
	}
}

func TestJSONEditor(t *testing.T) {
	out, err := NewJSONEditor([]byte(`{"keep":{"b":1,"a":2},"drop":true}`)).
		Set("x.y", nil).
		SetRaw("big", []byte("9007199254740993")).
		Delete("drop").
		Delete("missing").
		Result()
	require.NoError(t, err)
	assert.Equal(t, `{"b":1,"a":2}`, gjson.GetBytes(out, "keep").Raw)
	assert.Equal(t, gjson.Null, gjson.GetBytes(out, "x.y").Type)
	assert.Equal(t, "9007199254740993", gjson.GetBytes(out, "big").Raw, "raw fragments must not go through float64")
	assert.False(t, gjson.GetBytes(out, "drop").Exists())

	// The first error is recorded and later operations are skipped.
	_, err = NewJSONEditor([]byte(`{}`)).Set("", 1).Set("ok", 1).Result()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set")
}

func TestFindDuplicateTopLevelKey(t *testing.T) {
	controlled := []string{"max_tokens", "stream", "kv_transfer_params"}

	key, dup := FindDuplicateTopLevelKey([]byte(nestedBody), controlled)
	assert.False(t, dup)
	assert.Empty(t, key)

	// Non-controlled duplicates are allowed.
	key, dup = FindDuplicateTopLevelKey([]byte(`{"extra":1,"extra":2,"max_tokens":1}`), controlled)
	assert.False(t, dup)
	assert.Empty(t, key)

	// The first duplicated key in controlled order is reported, not the first in body order.
	key, dup = FindDuplicateTopLevelKey([]byte(`{"stream":true,"stream":false,"max_tokens":1,"max_tokens":2}`), controlled)
	assert.True(t, dup)
	assert.Equal(t, "max_tokens", key)

	// Nested occurrences do not count.
	key, dup = FindDuplicateTopLevelKey([]byte(`{"kv_transfer_params":{"stream":1,"stream":2},"stream":true}`), controlled)
	assert.False(t, dup)
	assert.Empty(t, key)

	key, dup = FindDuplicateTopLevelKey([]byte(`{"kv_transfer_params":{},"kv_transfer_params":{"remote_host":"x"}}`), controlled)
	assert.True(t, dup)
	assert.Equal(t, "kv_transfer_params", key)
}
