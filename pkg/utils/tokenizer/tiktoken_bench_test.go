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

package tokenizer

import (
	"math"
	"strings"
	"testing"
)

const benchSentence = "The quick brown fox jumps over the lazy dog while the diligent engineer measures tokenization latency under sustained load. "

// buildPrompt grows benchSentence until it tokenizes to about targetTokens.
func buildPrompt(tb testing.TB, tk Tokenizer, targetTokens int) string {
	tb.Helper()
	reps := 10
	for i := 0; i < 40; i++ {
		text := strings.Repeat(benchSentence, reps)
		out, err := tk.TokenizeInputText(text)
		if err != nil {
			tb.Fatal(err)
		}
		count := len(out) / 4
		if math.Abs(float64(count-targetTokens)) <= float64(targetTokens)*0.005 {
			return text
		}
		next := int(math.Round(float64(reps) * float64(targetTokens) / float64(count)))
		if next < 1 {
			next = 1
		}
		if next == reps {
			return text
		}
		reps = next
	}
	return strings.Repeat(benchSentence, reps)
}

// BenchmarkTokenizeInputText measures the local tiktoken tokenizer at the
// prompt sizes used in the gateway CPU sizing discussion (issue #2019).
func BenchmarkTokenizeInputText(b *testing.B) {
	for _, tc := range []struct {
		name   string
		tokens int
	}{
		{"1K", 1024},
		{"4K", 4096},
		{"16K", 16384},
		{"64K", 65536},
	} {
		b.Run(tc.name, func(b *testing.B) {
			tk := NewTiktokenTokenizer()
			text := buildPrompt(b, tk, tc.tokens)
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := tk.TokenizeInputText(text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
