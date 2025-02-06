package utils

import (
	"testing"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/stretchr/testify/assert"
)

func TestTokenizeInputText(t *testing.T) {
	inputStr := "Hello World, 你好世界"
	tokens, err := TokenizeInputText(inputStr)
	assert.Equal(t, nil, err)

	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, _ := tiktoken.GetEncoding(encoding)
	outputStr := tke.Decode(tokens)
	assert.Equal(t, inputStr, outputStr)
	assert.Equal(t, 1, 0)
}
