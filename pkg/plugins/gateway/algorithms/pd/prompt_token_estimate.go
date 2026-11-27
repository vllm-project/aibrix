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

	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// bytesPerTokenEstimate is the prompt-size heuristic used when the router
	// has no token count for the request: one token per four bytes of request
	// body, the same estimate the vLLM PD proxy examples use.
	bytesPerTokenEstimate = 4

	// DefaultTokenLoadImageTokens is the default per-image surcharge for the
	// token_load body estimate. Media payload bytes are excluded from the
	// four-bytes-per-token count, so inline base64 no longer dominates it.
	DefaultTokenLoadImageTokens = 1024

	// DefaultTokenLoadAudioTokens is the default per-audio-part surcharge.
	DefaultTokenLoadAudioTokens = 1024

	// DefaultTokenLoadVideoTokens is the default per-video-part surcharge.
	DefaultTokenLoadVideoTokens = 2048
)

type promptTokenEstimateConfig struct {
	imageTokens int
	audioTokens int
	videoTokens int
}

var defaultPromptTokenEstimateConfig = loadPromptTokenEstimateConfig()

func loadPromptTokenEstimateConfig() promptTokenEstimateConfig {
	return promptTokenEstimateConfig{
		imageTokens: utils.LoadEnvNonNegativeInt("AIBRIX_TOKEN_LOAD_IMAGE_TOKENS", DefaultTokenLoadImageTokens),
		audioTokens: utils.LoadEnvNonNegativeInt("AIBRIX_TOKEN_LOAD_AUDIO_TOKENS", DefaultTokenLoadAudioTokens),
		videoTokens: utils.LoadEnvNonNegativeInt("AIBRIX_TOKEN_LOAD_VIDEO_TOKENS", DefaultTokenLoadVideoTokens),
	}
}

// EstimatePromptTokens estimates the prompt length of a request from its body
// size when no token count is available. Text-only requests keep the original
// one-token-per-four-bytes heuristic. For chat content parts, the media URL or
// inline payload is removed from the byte count and a configurable per-part
// token estimate is added instead.
func EstimatePromptTokens(reqBody []byte) int {
	return estimatePromptTokensWithConfig(reqBody, defaultPromptTokenEstimateConfig)
}

func estimatePromptTokensWithConfig(reqBody []byte, cfg promptTokenEstimateConfig) int {
	if len(reqBody) == 0 {
		return 0
	}
	if !hasChatMediaPayload(reqBody) {
		return len(reqBody) / bytesPerTokenEstimate
	}

	messages := gjson.GetBytes(reqBody, "messages")
	if !messages.IsArray() {
		return len(reqBody) / bytesPerTokenEstimate
	}

	mediaBytes, mediaTokens := 0, 0
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			field, tokens := mediaPayload(part, cfg)
			if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
				return true
			}
			// Raw includes the JSON quotes and any escaping, so it is exactly
			// the number of request-body bytes this payload occupies.
			mediaBytes += len(field.Raw)
			mediaTokens += tokens
			return true
		})
		return true
	})

	if mediaBytes == 0 {
		return len(reqBody) / bytesPerTokenEstimate
	}
	textBytes := len(reqBody) - mediaBytes
	if textBytes < 0 {
		textBytes = 0
	}
	return textBytes/bytesPerTokenEstimate + mediaTokens
}

// hasChatMediaPayload keeps the common text-only path out of the JSON walk.
// Base64 payloads cannot contain JSON quotes, so the key checks cannot be
// satisfied by the payload itself.
func hasChatMediaPayload(body []byte) bool {
	if !bytes.Contains(body, []byte(`"messages"`)) {
		return false
	}
	return bytes.Contains(body, []byte(`"image_url"`)) ||
		bytes.Contains(body, []byte(`"audio_url"`)) ||
		bytes.Contains(body, []byte(`"input_audio"`)) ||
		bytes.Contains(body, []byte(`"video_url"`))
}

func mediaPayload(part gjson.Result, cfg promptTokenEstimateConfig) (gjson.Result, int) {
	switch part.Get("type").String() {
	case "image_url":
		field := part.Get("image_url.url")
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			field = part.Get("image_url")
		}
		return field, max(cfg.imageTokens, 0)
	case "audio_url":
		field := part.Get("audio_url.url")
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			field = part.Get("audio_url")
		}
		return field, max(cfg.audioTokens, 0)
	case "input_audio":
		field := part.Get("input_audio.data")
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			field = part.Get("input_audio.url")
		}
		return field, max(cfg.audioTokens, 0)
	case "video_url":
		field := part.Get("video_url.url")
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			field = part.Get("video_url")
		}
		return field, max(cfg.videoTokens, 0)
	default:
		return gjson.Result{}, 0
	}
}
