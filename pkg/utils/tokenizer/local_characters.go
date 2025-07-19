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

// CharacterTokenizer implements local tokenization by converting text to bytes
type CharacterTokenizer struct{}

func (s *CharacterTokenizer) TokenizeInputText(text string) ([]byte, error) {
	// Note: For some characters such as non-english letters or emoji's, one character may convert to multiple bytes.
	// which may split across different token blocks. It does not impact prefix-match technically but characters
	// may loose theoretical meaning.
	// TODO: evaluate if text conversion can be done to []rune and then convert []rune to []byte with minimal overhead.
	return []byte(text), nil
}
