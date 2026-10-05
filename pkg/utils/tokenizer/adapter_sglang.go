/*
Copyright 2025 The Aibrix Team.

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

// sglangAdapter implements engineAdapter for SGLang inference engine
// Note: SGLang does not provide tokenize/detokenize endpoints
type sglangAdapter struct {
	model string
}

// newSGLangAdapter creates a new SGLang adapter
func newSGLangAdapter(model string) *sglangAdapter {
	return &sglangAdapter{
		model: model,
	}
}

// GetTokenizePath returns empty as SGLang doesn't support tokenization
func (a *sglangAdapter) GetTokenizePath() string {
	return ""
}

// GetDetokenizePath returns empty as SGLang doesn't support detokenization
func (a *sglangAdapter) GetDetokenizePath() string {
	return ""
}

// SupportsTokenization returns false as SGLang doesn't support tokenization
func (a *sglangAdapter) SupportsTokenization() bool {
	return false
}

// SupportsDetokenization returns false as SGLang doesn't support detokenization
func (a *sglangAdapter) SupportsDetokenization() bool {
	return false
}

// SupportsChat returns false as SGLang doesn't support chat tokenization
func (a *sglangAdapter) SupportsChat() bool {
	return false
}

// PrepareTokenizeRequest is not supported by SGLang
func (a *sglangAdapter) PrepareTokenizeRequest(input TokenizeInput) (interface{}, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "tokenization",
	}
}

// PrepareDetokenizeRequest is not supported by SGLang
func (a *sglangAdapter) PrepareDetokenizeRequest(tokens []int) (interface{}, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "detokenization",
	}
}

// ParseTokenizeResponse is not supported by SGLang
func (a *sglangAdapter) ParseTokenizeResponse(data []byte) (*TokenizeResult, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "tokenize response parsing",
	}
}

// ParseDetokenizeResponse is not supported by SGLang
func (a *sglangAdapter) ParseDetokenizeResponse(data []byte) (string, error) {
	return "", ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "detokenize response parsing",
	}
}
