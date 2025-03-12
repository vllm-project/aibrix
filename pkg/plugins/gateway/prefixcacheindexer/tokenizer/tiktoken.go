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

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// https://cookbook.openai.com/examples/how_to_count_tokens_with_tiktoken
const encoding = "cl100k_base"

type tiktokenTokenizer struct{}

func NewTiktokenTokenizer() Tokenizer {
	return &tiktokenTokenizer{}
}

func (s tiktokenTokenizer) TokenizeInputText(text string) ([]byte, error) {
	// if you don't want download dictionary at runtime, you can use offline loader
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}

	// encode
	token := tke.Encode(text, nil, nil)
	return intToByteArray(token), nil
}

func intToByteArray(intArray []int) []byte {
	var buf bytes.Buffer
	for _, num := range intArray {
		err := binary.Write(&buf, binary.BigEndian, int32(num))
		if err != nil {
			fmt.Println("binary.Write failed:", err)
		}
	}
	return buf.Bytes()
}
