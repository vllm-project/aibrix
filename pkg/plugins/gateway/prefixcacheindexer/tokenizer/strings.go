package tokenizer

import "strings"

type stringTokenizer struct{}

func NewStringTokenizer() Tokenizer {
	return &stringTokenizer{}
}

func (s stringTokenizer) TokenizeInputText(text string) ([]byte, error) {
	return convertStrArrayToByteArray(strings.Split(text, " ")), nil
}

func convertStrArrayToByteArray(tokens []string) []byte {
	var x = []byte{}
	for i := 0; i < len(tokens); i++ {
		b := []byte(tokens[i])
		for j := 0; j < len(b); j++ {
			x = append(x, b[j])
		}
	}
	return x
}
