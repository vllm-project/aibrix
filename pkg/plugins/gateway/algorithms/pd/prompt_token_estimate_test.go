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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func chatBodyWithContentParts(parts ...string) []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`)
}

func TestEstimatePromptTokens_TextOnlyUnchanged(t *testing.T) {
	assert.Equal(t, 0, EstimatePromptTokens(nil))
	assert.Equal(t, 0, EstimatePromptTokens([]byte("abc")))
	assert.Equal(t, 25, EstimatePromptTokens(bytes.Repeat([]byte("x"), 100)))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":128}`)
	assert.Equal(t, len(body)/bytesPerTokenEstimate, EstimatePromptTokens(body))

	parts := chatBodyWithContentParts(`{"type":"text","text":"hello"}`)
	assert.Equal(t, len(parts)/bytesPerTokenEstimate, EstimatePromptTokens(parts))
}

func TestEstimatePromptTokens_MediaPayloads(t *testing.T) {
	cfg := promptTokenEstimateConfig{imageTokens: 100, audioTokens: 50, videoTokens: 200}
	tests := []struct {
		name    string
		part    string
		payload string
		tokens  int
	}{
		{
			name:    "inline image",
			part:    `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`,
			payload: "data:image/png;base64,AAAA",
			tokens:  cfg.imageTokens,
		},
		{
			name:    "remote image",
			part:    `{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}`,
			payload: "https://example.com/a.png",
			tokens:  cfg.imageTokens,
		},
		{
			name:    "image url string form",
			part:    `{"type":"image_url","image_url":"https://example.com/a.png"}`,
			payload: "https://example.com/a.png",
			tokens:  cfg.imageTokens,
		},
		{
			name:    "audio url",
			part:    `{"type":"audio_url","audio_url":{"url":"https://example.com/a.wav"}}`,
			payload: "https://example.com/a.wav",
			tokens:  cfg.audioTokens,
		},
		{
			name:    "audio url string form",
			part:    `{"type":"audio_url","audio_url":"https://example.com/a.wav"}`,
			payload: "https://example.com/a.wav",
			tokens:  cfg.audioTokens,
		},
		{
			name:    "input audio data",
			part:    `{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}`,
			payload: "QUJD",
			tokens:  cfg.audioTokens,
		},
		{
			name:    "input audio empty data falls back to url",
			part:    `{"type":"input_audio","input_audio":{"data":"","url":"https://example.com/a.wav"}}`,
			payload: "https://example.com/a.wav",
			tokens:  cfg.audioTokens,
		},
		{
			name:    "input audio url fallback",
			part:    `{"type":"input_audio","input_audio":{"url":"https://example.com/a.wav"}}`,
			payload: "https://example.com/a.wav",
			tokens:  cfg.audioTokens,
		},
		{
			name:    "video url",
			part:    `{"type":"video_url","video_url":{"url":"https://example.com/a.mp4"}}`,
			payload: "https://example.com/a.mp4",
			tokens:  cfg.videoTokens,
		},
		{
			name:    "video url string form",
			part:    `{"type":"video_url","video_url":"https://example.com/a.mp4"}`,
			payload: "https://example.com/a.mp4",
			tokens:  cfg.videoTokens,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := chatBodyWithContentParts(tt.part)
			// gjson.Raw includes the two JSON quotes around the string.
			want := (len(body)-len(tt.payload)-2)/bytesPerTokenEstimate + tt.tokens
			assert.Equal(t, want, estimatePromptTokensWithConfig(body, cfg))
		})
	}
}

func TestEstimatePromptTokens_InlineImagePayloadSizeDoesNotChangeEstimate(t *testing.T) {
	cfg := promptTokenEstimateConfig{imageTokens: DefaultTokenLoadImageTokens}
	estimate := func(payload string) int {
		body := chatBodyWithContentParts(`{"type":"image_url","image_url":{"url":"` + payload + `"}}`)
		return estimatePromptTokensWithConfig(body, cfg)
	}

	small := estimate("data:image/png;base64," + strings.Repeat("A", 128))
	large := estimate("data:image/png;base64," + strings.Repeat("A", 1<<20))
	assert.Equal(t, small, large, "base64 length must not dominate the token estimate")
}

func TestEstimatePromptTokens_MultipleMessagesAndParts(t *testing.T) {
	cfg := promptTokenEstimateConfig{imageTokens: 100, audioTokens: 50, videoTokens: 200}
	image := "data:image/png;base64,AAAA"
	audio := "QUJD"
	video := "https://example.com/a.mp4"
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"` + image + `"}},{"type":"input_audio","input_audio":{"data":"` + audio + `"}}]},` +
		`{"role":"user","content":[{"type":"video_url","video_url":{"url":"` + video + `"}}]}]}`)

	mediaBytes := len(image) + 2 + len(audio) + 2 + len(video) + 2
	want := (len(body)-mediaBytes)/bytesPerTokenEstimate + cfg.imageTokens + cfg.audioTokens + cfg.videoTokens
	assert.Equal(t, want, estimatePromptTokensWithConfig(body, cfg))
}

func TestEstimatePromptTokens_FallbackCases(t *testing.T) {
	cfg := promptTokenEstimateConfig{imageTokens: 100, audioTokens: 50, videoTokens: 200}
	cases := map[string][]byte{
		"malformed json":          []byte(`{"messages":[`),
		"not an object":           []byte(`[]`),
		"no messages":             []byte(`{"prompt":"hello"}`),
		"messages not an array":   []byte(`{"messages":{"content":"hello"}}`),
		"content is a string":     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		"unknown content part":    chatBodyWithContentParts(`{"type":"text","data":"AAAA"}`),
		"empty media value":       chatBodyWithContentParts(`{"type":"image_url","image_url":{"url":""}}`),
		"media outside messages":  []byte(`{"model":"m","metadata":{"image_url":{"url":"data:image/png;base64,AAAA"}}}`),
		"media key only in tools": []byte(`{"model":"m","tools":[{"type":"function","function":{"parameters":{"type":"object","properties":{"image_url":{"type":"string"}}}}}]}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, len(body)/bytesPerTokenEstimate, estimatePromptTokensWithConfig(body, cfg))
		})
	}
}

func TestEstimatePromptTokens_ConfigOverrides(t *testing.T) {
	payload := "data:image/png;base64,AAAA"
	body := chatBodyWithContentParts(`{"type":"image_url","image_url":{"url":"` + payload + `"}}`)
	base := (len(body) - len(payload) - 2) / bytesPerTokenEstimate

	assert.Equal(t, base+512, estimatePromptTokensWithConfig(body, promptTokenEstimateConfig{imageTokens: 512}))
	assert.Equal(t, base, estimatePromptTokensWithConfig(body, promptTokenEstimateConfig{imageTokens: 0}))
	assert.Equal(t, base, estimatePromptTokensWithConfig(body, promptTokenEstimateConfig{imageTokens: -10}), "negative estimates are clamped to zero")
}

func TestLoadPromptTokenEstimateConfig(t *testing.T) {
	t.Run("overrides", func(t *testing.T) {
		t.Setenv("AIBRIX_TOKEN_LOAD_IMAGE_TOKENS", "512")
		t.Setenv("AIBRIX_TOKEN_LOAD_AUDIO_TOKENS", "0")
		t.Setenv("AIBRIX_TOKEN_LOAD_VIDEO_TOKENS", "4096")

		cfg := loadPromptTokenEstimateConfig()
		assert.Equal(t, 512, cfg.imageTokens)
		assert.Equal(t, 0, cfg.audioTokens)
		assert.Equal(t, 4096, cfg.videoTokens)
	})

	t.Run("invalid values fall back", func(t *testing.T) {
		t.Setenv("AIBRIX_TOKEN_LOAD_IMAGE_TOKENS", "-1")
		t.Setenv("AIBRIX_TOKEN_LOAD_AUDIO_TOKENS", "not-a-number")
		t.Setenv("AIBRIX_TOKEN_LOAD_VIDEO_TOKENS", "")

		cfg := loadPromptTokenEstimateConfig()
		assert.Equal(t, DefaultTokenLoadImageTokens, cfg.imageTokens)
		assert.Equal(t, DefaultTokenLoadAudioTokens, cfg.audioTokens)
		assert.Equal(t, DefaultTokenLoadVideoTokens, cfg.videoTokens)
	})
}

func BenchmarkEstimatePromptTokens(b *testing.B) {
	text := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":128}`)
	image := chatBodyWithContentParts(`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 1<<16) + `"}}`)

	b.Run("text", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = EstimatePromptTokens(text)
		}
	})
	b.Run("image", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = EstimatePromptTokens(image)
		}
	})
}
