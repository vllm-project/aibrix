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
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// setIncludeTools sets AIBRIX_PREFIX_CACHE_INCLUDE_TOOLS and reloads the flag through the
// real env loader, restoring the previous value when the test ends.
func setIncludeTools(t *testing.T, enabled bool) {
	t.Helper()
	t.Setenv(envPrefixCacheIncludeTools, strconv.FormatBool(enabled))
	previous := prefixCacheIncludeTools
	prefixCacheIncludeTools = utils.LoadEnvBool(envPrefixCacheIncludeTools, true)
	require.Equal(t, enabled, prefixCacheIncludeTools)
	t.Cleanup(func() { prefixCacheIncludeTools = previous })
}

func chatRoutingMessage(t *testing.T, body string) string {
	t.Helper()
	_, message, _, errRes := validateRequestBody("test-request-id", PathChatCompletions, []byte(body), utils.User{})
	require.Nil(t, errRes, "unexpected error response for body: %s", body)
	return message
}

const (
	toolsTestMessages = `"messages":[{"role":"system","content":"you are a helpful assistant"},{"role":"user","content":[{"type":"text","text":"what is the weather?"}]}]`
	// toolsTestMessagesText is the routing message built from toolsTestMessages alone.
	toolsTestMessagesText = `you are a helpful assistant [{"type":"text","text":"what is the weather?"}]`
	toolsTestWeather      = `[{"type":"function","function":{"name":"get_weather","description":"Get the weather","parameters":{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer","maximum":1e1}}}}}]`
	// toolsTestWeatherCanonical is toolsTestWeather with keys sorted at every level.
	toolsTestWeatherCanonical = `[{"function":{"description":"Get the weather","name":"get_weather","parameters":{"properties":{"city":{"type":"string"},"days":{"maximum":1e1,"type":"integer"}},"type":"object"}},"type":"function"}]`
)

func TestChatRoutingMessage_NoToolsUnchanged(t *testing.T) {
	setIncludeTools(t, true)

	cases := map[string]string{
		"absent":                  `{"model":"m",` + toolsTestMessages + `}`,
		"null":                    `{"model":"m",` + toolsTestMessages + `,"tools":null}`,
		"empty array":             `{"model":"m",` + toolsTestMessages + `,"tools":[]}`,
		"empty array whitespace":  `{"model":"m",` + toolsTestMessages + `,"tools":[ ]}`,
		"empty array before msgs": `{"model":"m","tools":[],` + toolsTestMessages + `}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, toolsTestMessagesText, chatRoutingMessage(t, body))
		})
	}
}

func TestChatRoutingMessage_ToolsPrecedeMessages(t *testing.T) {
	setIncludeTools(t, true)

	body := `{"model":"m",` + toolsTestMessages + `,"tools":` + toolsTestWeather + `}`
	assert.Equal(t, toolsTestWeatherCanonical+" "+toolsTestMessagesText, chatRoutingMessage(t, body))

	// Anthropic-style /v1/messages requests share the chat parser; the rendering does not
	// depend on the tool schema.
	anthropic := `{"model":"m",` + toolsTestMessages + `,"tools":[{"name":"get_weather","input_schema":{"type":"object"},"description":"Get the weather"}]}`
	_, message, _, errRes := validateRequestBody("test-request-id", PathMessages, []byte(anthropic), utils.User{})
	require.Nil(t, errRes)
	assert.Equal(t, `[{"description":"Get the weather","input_schema":{"type":"object"},"name":"get_weather"}] `+toolsTestMessagesText, message)
}

func TestChatRoutingMessage_DifferentToolsDiffer(t *testing.T) {
	setIncludeTools(t, true)

	toolsB := strings.Replace(toolsTestWeather, "Get the weather", "Get the forecast", 1)
	bodyA := `{"model":"m",` + toolsTestMessages + `,"tools":` + toolsTestWeather + `}`
	bodyB := `{"model":"m",` + toolsTestMessages + `,"tools":` + toolsB + `}`
	messageA, messageB := chatRoutingMessage(t, bodyA), chatRoutingMessage(t, bodyB)
	require.NotEqual(t, messageA, messageB)

	// The two messages diverge early inside the tools block, so only the few blocks
	// ahead of the differing description match. Without tools they would be identical.
	tok := tokenizer.NewCharacterTokenizer()
	indexer := prefixcacheindexer.NewPrefixHashTable()
	tokensA, err := tok.TokenizeInputText(messageA)
	require.NoError(t, err)
	tokensB, err := tok.TokenizeInputText(messageB)
	require.NoError(t, err)
	indexer.AddPrefix(indexer.GetPrefixHashes(tokensA), "m", "pod-a")
	matched, _ := indexer.MatchPrefix(tokensB, "m", map[string]struct{}{"pod-a": {}})
	assert.Less(t, matched["pod-a"], 50, "requests with different tools must not look like a full prefix match")

	matched, _ = indexer.MatchPrefix(tokensA, "m", map[string]struct{}{"pod-a": {}})
	assert.Equal(t, 100, matched["pod-a"], "an identical request must still fully match")
}

func TestChatRoutingMessage_ToolsHTMLNotEscaped(t *testing.T) {
	setIncludeTools(t, true)

	body := `{"model":"m",` + toolsTestMessages + `,"tools":[{"type":"function","function":{"name":"f","description":"a < b && c > d"}}]}`
	message := chatRoutingMessage(t, body)
	assert.Contains(t, message, `"a < b && c > d"`)
	assert.NotContains(t, message, "\\u003c", "HTML characters must not be escaped")
}

// writeShuffledJSON serializes v with object keys in a random order and random
// insignificant whitespace, standing in for clients that emit the same tools differently.
func writeShuffledJSON(b *strings.Builder, v interface{}, r *rand.Rand) {
	ws := func() {
		b.WriteString([]string{"", " ", "\n  ", "\t"}[r.Intn(4)])
	}
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			ws()
			kb, _ := json.Marshal(k)
			b.Write(kb)
			ws()
			b.WriteByte(':')
			ws()
			writeShuffledJSON(b, val[k], r)
		}
		ws()
		b.WriteByte('}')
	case []interface{}:
		b.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			ws()
			writeShuffledJSON(b, e, r)
		}
		ws()
		b.WriteByte(']')
	default:
		vb, _ := json.Marshal(val)
		b.Write(vb)
	}
}

func TestChatRoutingMessage_ToolsKeyOrderIndependent(t *testing.T) {
	setIncludeTools(t, true)

	const tools = `[
		{"type":"function","function":{"name":"get_weather","description":"Get the weather","strict":true,
			"parameters":{"type":"object","required":["city"],"additionalProperties":false,
				"properties":{"city":{"type":"string","description":"City name"},"days":{"type":"integer","minimum":1},"unit":{"type":"string","enum":["c","f"]}}}}},
		{"type":"function","function":{"name":"search","description":"Search the web",
			"parameters":{"type":"object","properties":{"query":{"type":"string"},"top_k":{"type":"number","default":0.5}}}}}
	]`
	var parsed interface{}
	d := json.NewDecoder(strings.NewReader(tools))
	d.UseNumber()
	require.NoError(t, d.Decode(&parsed))

	expected := chatRoutingMessage(t, `{"model":"m",`+toolsTestMessages+`,"tools":`+tools+`}`)
	require.True(t, strings.HasSuffix(expected, " "+toolsTestMessagesText))

	r := rand.New(rand.NewSource(1))
	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		var b strings.Builder
		writeShuffledJSON(&b, parsed, r)
		seen[b.String()] = struct{}{}
		body := `{"model":"m",` + toolsTestMessages + `,"tools":` + b.String() + `}`
		require.Equal(t, expected, chatRoutingMessage(t, body), "tools: %s", b.String())
	}
	require.Greater(t, len(seen), 1, "the permutations must actually vary the input")
}

func TestChatRoutingMessage_ToolsDisabled(t *testing.T) {
	setIncludeTools(t, false)

	body := `{"model":"m",` + toolsTestMessages + `,"tools":` + toolsTestWeather + `}`
	assert.Equal(t, toolsTestMessagesText, chatRoutingMessage(t, body))
	assert.Equal(t, "", canonicalToolsText("test-request-id", json.RawMessage(toolsTestWeather)))
}

func TestCanonicalToolsText_InvalidFallsBackToRaw(t *testing.T) {
	setIncludeTools(t, true)

	assert.Equal(t, `[{"type":`, canonicalToolsText("test-request-id", json.RawMessage(` [{"type": `)))
}
