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
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The expected values below were produced by CPython 3.12:
//
//	json.dumps(json.loads(doc), ensure_ascii=False, separators=(",", ":"))
func Test_renderJSONLikePython(t *testing.T) {
	testCases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "unicode escape is decoded and an exponent float is normalized",
			doc:  `{"text":"你","n":1e0}`,
			want: `{"text":"你","n":1.0}`,
		},
		{
			name: "escaped and raw non-ASCII render identically, including astral characters",
			doc:  `["é","é","😀","😀"]`,
			want: `["é","é","😀","😀"]`,
		},
		{
			name: "control characters keep Python's escapes; DEL and U+2028 are emitted raw",
			doc:  `{"a":"x\u0000y\u001f\b\f\n\r\t\"\\\/","d":"\u007f "}`,
			want: `{"a":"x\u0000y\u001f\b\f\n\r\t\"\\/","d":"` + "\u007f " + `"}`,
		},
		{
			name: "key order is preserved and whitespace is dropped",
			doc:  ` { "z": 1, "a": 2, "m": { "y": [ ], "b": { } } } `,
			want: `{"z":1,"a":2,"m":{"y":[],"b":{}}}`,
		},
		{
			name: "HTML characters are not escaped",
			doc:  `{"<>&":"<>&"}`,
			want: `{"<>&":"<>&"}`,
		},
		{
			name: "literals",
			doc:  `[true,false,null," "]`,
			want: `[true,false,null," "]`,
		},
		{
			name: "integers keep their digits at any size; negative zero is zero",
			doc:  `[0,10,-0,12345678901234567890123]`,
			want: `[0,10,0,12345678901234567890123]`,
		},
		{
			name: "floats use Python's repr layout",
			doc:  `[1E5,1.50,-0.0,100.0,0.1,0.0001,1e-5,1e15,1e16,1.2345678901234568e17,5e-324,1.7976931348623157e308,123456789.123456789]`,
			want: `[100000.0,1.5,-0.0,100.0,0.1,0.0001,1e-05,1000000000000000.0,1e+16,1.2345678901234568e+17,5e-324,1.7976931348623157e+308,123456789.12345679]`,
		},
		{
			name: "float overflow becomes Infinity and underflow becomes zero",
			doc:  `[1e400,-1e400,1e-400]`,
			want: `[Infinity,-Infinity,0.0]`,
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderJSONLikePython([]byte(tt.doc))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A parse failure is logged with its reason only: sonic's error text quotes the body around the
// failure, which is customer content.
func Test_jsonParseReason_OmitsBodyContent(t *testing.T) {
	const secret = "SECRET" // short enough to fit in the excerpt sonic quotes
	var req struct {
		Model string `json:"model"`
	}
	for name, body := range map[string]string{
		"syntax error":  `{"model":"m","input":"` + secret + `" "oops":1}`,
		"type mismatch": `{"model":{"leak":"` + secret + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := sonic.Unmarshal([]byte(body), &req)
			require.Error(t, err)
			require.Contains(t, err.Error(), secret, "premise: sonic's own error text quotes the body")

			reason := jsonParseReason(err)
			assert.NotEmpty(t, reason)
			assert.NotContains(t, reason, secret)
		})
	}
}

func Test_renderJSONLikePython_Invalid(t *testing.T) {
	for name, doc := range map[string]string{
		"truncated":      `{"a":`,
		"trailing comma": `[1,]`,
		"bare NaN":       `[NaN]`,
		"too deep":       strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderJSONLikePython([]byte(doc))
			assert.Error(t, err)
		})
	}
}
