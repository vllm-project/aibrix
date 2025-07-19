/*
Copyright 2024 The Aibrix Team.

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

// SGLangAdapter implements EngineAdapter for SGLang inference engine
// Note: SGLang does not provide tokenize/detokenize endpoints
type SGLangAdapter struct {
	model string
}

// NewSGLangAdapter creates a new SGLang adapter
func NewSGLangAdapter(model string) *SGLangAdapter {
	return &SGLangAdapter{
		model: model,
	}
}

// GetTokenizePath returns empty as SGLang doesn't support tokenization
func (a *SGLangAdapter) GetTokenizePath() string {
	return ""
}

// GetDetokenizePath returns empty as SGLang doesn't support detokenization
func (a *SGLangAdapter) GetDetokenizePath() string {
	return ""
}

// SupportsTokenization returns false as SGLang doesn't support tokenization
func (a *SGLangAdapter) SupportsTokenization() bool {
	return false
}

// SupportsDetokenization returns false as SGLang doesn't support detokenization
func (a *SGLangAdapter) SupportsDetokenization() bool {
	return false
}

// SupportsChat returns false as SGLang doesn't support chat tokenization
func (a *SGLangAdapter) SupportsChat() bool {
	return false
}

// PrepareTokenizeRequest is not supported by SGLang
func (a *SGLangAdapter) PrepareTokenizeRequest(input TokenizeInput) (interface{}, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "tokenization",
	}
}

// PrepareDetokenizeRequest is not supported by SGLang
func (a *SGLangAdapter) PrepareDetokenizeRequest(tokens []int) (interface{}, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "detokenization",
	}
}

// ParseTokenizeResponse is not supported by SGLang
func (a *SGLangAdapter) ParseTokenizeResponse(data []byte) (*TokenizeResult, error) {
	return nil, ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "tokenize response parsing",
	}
}

// ParseDetokenizeResponse is not supported by SGLang
func (a *SGLangAdapter) ParseDetokenizeResponse(data []byte) (string, error) {
	return "", ErrUnsupportedOperation{
		Engine:    "sglang",
		Operation: "detokenize response parsing",
	}
}
