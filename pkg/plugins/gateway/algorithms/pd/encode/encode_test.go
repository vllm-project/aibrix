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

package encode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestEncoderRequestCarriesOneItemVerbatim(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"https://x/cat.png"},  "uuid":"u1"}`
	audio := `{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}`
	body := []byte(`{"model":"qwen-vl","stream":true,"max_tokens":64,
		"mm_processor_kwargs":{"max_pixels":1024},
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":[{"type":"text","text":"what is this?"},` + image + `]},
			{"role":"user","content":[` + audio + `]}]}`)

	items := MediaItems(body)
	require.Equal(t, []string{image, audio}, items)

	req, err := requestBody(body, items[0])
	require.NoError(t, err)
	assert.Equal(t, `"qwen-vl"`, gjson.GetBytes(req, "model").Raw)
	assert.Equal(t, "false", gjson.GetBytes(req, "stream").Raw)
	assert.Equal(t, `{"max_pixels":1024}`, gjson.GetBytes(req, "mm_processor_kwargs").Raw)
	assert.Equal(t, "user", gjson.GetBytes(req, "messages.0.role").String())
	// Byte for byte, so the encoder and the prefill instance hash the same item.
	assert.Equal(t, `[`+image+`]`, gjson.GetBytes(req, "messages.0.content").Raw)
	assert.False(t, gjson.GetBytes(req, "max_tokens").Exists())

	assert.Empty(t, MediaItems([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
}
