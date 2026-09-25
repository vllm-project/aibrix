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
	"bytes"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

// prefixCacheIncludeTools is loaded once at startup from AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS.
// Many chat templates render the tool definitions ahead of the conversation, so the
// prompt the engine actually caches starts with the tools block. Without tools in the
// prefix-match text, two requests that share their messages but carry different tools
// look like a full prefix match. It is atomic only so tests can flip it safely.
var prefixCacheIncludeTools atomic.Bool

func init() {
	prefixCacheIncludeTools.Store(utils.LoadEnvBool(constants.EnvPrefixCacheIncludeTools, true))
}

// canonicalJSON re-encodes JSON values deterministically: object keys are sorted at every
// level, output is compact, HTML characters are not escaped (matching the `tojson` filter
// used by chat templates) and numbers keep their original spelling.
var canonicalJSON = sonic.Config{
	SortMapKeys: true,
	UseNumber:   true,
	EscapeHTML:  false,
}.Froze()

// prefixMatchText returns the text prefix-matching policies should hash for a chat
// request: the canonical tools text, a single space, then the messages text. It returns
// "" when tools do not contribute, in which case the policies use the messages text
// (RoutingContext.Message) unchanged.
func prefixMatchText(requestID string, tools json.RawMessage, message string) string {
	toolsText := canonicalToolsText(requestID, tools)
	return combinePrefixText(message, toolsText)
}

func combinePrefixText(message string, fields ...string) string {
	parts := make([]string, 0, len(fields)+1)
	for _, field := range fields {
		if field != "" {
			parts = append(parts, field)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	parts = append(parts, message)
	return strings.Join(parts, " ")
}

func canonicalRequestFieldText(requestID string, field json.RawMessage) string {
	raw := bytes.TrimSpace(field)
	if len(raw) == 0 || bytes.Equal(raw, []byte(jsonNull)) {
		return ""
	}

	var value interface{}
	if err := canonicalJSON.Unmarshal(raw, &value); err != nil {
		klog.V(4).InfoS("failed to canonicalize request field, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	canonical, err := canonicalJSON.Marshal(value)
	if err != nil {
		klog.V(4).InfoS("failed to canonicalize request field, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	return string(canonical)
}

// canonicalToolsText renders the raw "tools" value of a chat request. It returns "" when
// tools must not contribute: the feature is disabled, or the field is absent, null, an
// empty array or not an array at all. The rendering is schema-agnostic, so it works for
// both OpenAI and Anthropic style tool definitions.
func canonicalToolsText(requestID string, tools json.RawMessage) string {
	if !prefixCacheIncludeTools.Load() {
		return ""
	}
	raw := bytes.TrimSpace(tools)
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}

	var v []interface{}
	if err := canonicalJSON.Unmarshal(raw, &v); err != nil {
		// Never reject a request over its tools: fall back to the bytes as sent.
		klog.V(4).InfoS("failed to canonicalize tools, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	if len(v) == 0 {
		return ""
	}
	canonical, err := canonicalJSON.Marshal(v)
	if err != nil {
		klog.V(4).InfoS("failed to canonicalize tools, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	return string(canonical)
}
