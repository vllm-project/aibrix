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

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

// envPrefixCacheIncludeTools controls whether the chat request "tools" field is part of
// the routing message used for prefix matching. Defaults to true.
const envPrefixCacheIncludeTools = "AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS"

// prefixCacheIncludeTools is read once at startup. Many chat templates render the tool
// definitions ahead of the conversation, so the prompt the engine actually caches starts
// with the tools block. Without tools in the routing message, two requests that share
// their messages but carry different tools look like a full prefix match.
var prefixCacheIncludeTools = utils.LoadEnvBool(envPrefixCacheIncludeTools, true)

// canonicalJSON re-encodes JSON values deterministically: object keys are sorted at every
// level, output is compact, HTML characters are not escaped (matching the `tojson` filter
// used by chat templates) and numbers keep their original spelling.
var canonicalJSON = sonic.Config{
	SortMapKeys: true,
	UseNumber:   true,
	EscapeHTML:  false,
}.Froze()

// canonicalToolsText renders the raw "tools" value of a chat request as the text that is
// prepended to the routing message. It returns "" when tools must not contribute: the
// feature is disabled, or the field is absent, null or an empty array. The rendering is
// schema-agnostic, so it works for both OpenAI and Anthropic style tool definitions.
func canonicalToolsText(requestID string, tools json.RawMessage) string {
	if !prefixCacheIncludeTools {
		return ""
	}
	raw := bytes.TrimSpace(tools)
	if len(raw) == 0 || string(raw) == jsonNull {
		return ""
	}

	var v interface{}
	if err := canonicalJSON.Unmarshal(raw, &v); err != nil {
		// Never reject a request over its tools: fall back to the bytes as sent.
		klog.V(4).InfoS("failed to canonicalize tools, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	if arr, ok := v.([]interface{}); ok && len(arr) == 0 {
		return ""
	}
	canonical, err := canonicalJSON.Marshal(v)
	if err != nil {
		klog.V(4).InfoS("failed to canonicalize tools, using raw bytes", "requestID", requestID, "error", err)
		return string(raw)
	}
	return string(canonical)
}
