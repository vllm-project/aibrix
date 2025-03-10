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
