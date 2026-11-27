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
	"unsafe"

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

// gjsonFieldNoCopy avoids copying reqBody into a string for gjson. The returned
// Result may reference the body's memory, so it must not outlive the body or be
// used while the body is mutated. The estimate consumes it synchronously, and
// the gateway does not mutate ReqBody during routing.
func gjsonFieldNoCopy(body []byte, path string) gjson.Result {
	return gjson.Get(unsafe.String(unsafe.SliceData(body), len(body)), path)
}

func estimatePromptTokensWithConfig(reqBody []byte, cfg promptTokenEstimateConfig) int {
	if len(reqBody) == 0 {
		return 0
	}
	if !hasMediaPayload(reqBody) {
		return len(reqBody) / bytesPerTokenEstimate
	}
	if !gjson.ValidBytes(reqBody) {
		return len(reqBody) / bytesPerTokenEstimate
	}

	mediaBytes, mediaTokens := 0, 0
	addPart := func(part gjson.Result) {
		field, tokens := mediaPayload(part, cfg)
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			return
		}
		// Raw includes the JSON quotes and any escaping, so it is exactly
		// the number of request-body bytes this payload occupies.
		mediaBytes += len(field.Raw)
		mediaTokens += tokens
	}
	scanContent := func(content gjson.Result) {
		if !content.IsArray() {
			return
		}
		content.ForEach(func(_, part gjson.Result) bool {
			addPart(part)
			return true
		})
	}

	messages := gjsonFieldNoCopy(reqBody, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			scanContent(message.Get("content"))
			return true
		})
	}

	// Responses API input can be a direct content-part array or message items
	// with their own content arrays.
	input := gjsonFieldNoCopy(reqBody, "input")
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "input_image" {
				addPart(item)
				return true
			}
			scanContent(item.Get("content"))
			return true
		})
	}

	if mediaBytes == 0 {
		return len(reqBody) / bytesPerTokenEstimate
	}
	textBytes := len(reqBody) - mediaBytes
	if textBytes < 0 {
		textBytes = 0
	}
	return textBytes/bytesPerTokenEstimate + mediaTokens
}

// hasMediaPayload keeps the common text-only path out of the JSON walk.
// A false positive only costs one parse; the walk below checks the actual
// messages[].content[] and Responses input parts, so text that merely mentions
// a media key is not treated as media.
func hasMediaPayload(body []byte) bool {
	if !bytes.Contains(body, []byte(`"messages"`)) && !bytes.Contains(body, []byte(`"input"`)) {
		return false
	}
	return bytes.Contains(body, []byte(`"image_url"`)) ||
		bytes.Contains(body, []byte(`"input_image"`)) ||
		bytes.Contains(body, []byte(`"audio_url"`)) ||
		bytes.Contains(body, []byte(`"input_audio"`)) ||
		bytes.Contains(body, []byte(`"video_url"`))
}

func mediaPayload(part gjson.Result, cfg promptTokenEstimateConfig) (gjson.Result, int) {
	switch part.Get("type").String() {
	case "image_url":
		return getMediaField(part, "image_url"), max(cfg.imageTokens, 0)
	case "input_image":
		return getMediaField(part, "image_url"), max(cfg.imageTokens, 0)
	case "audio_url":
		return getMediaField(part, "audio_url"), max(cfg.audioTokens, 0)
	case "input_audio":
		field := part.Get("input_audio.data")
		if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
			field = part.Get("input_audio.url")
		}
		return field, max(cfg.audioTokens, 0)
	case "video_url":
		return getMediaField(part, "video_url"), max(cfg.videoTokens, 0)
	default:
		return gjson.Result{}, 0
	}
}

func getMediaField(part gjson.Result, key string) gjson.Result {
	field := part.Get(key + ".url")
	if !field.Exists() || field.Type != gjson.String || len(field.Raw) <= 2 {
		field = part.Get(key)
	}
	return field
}
