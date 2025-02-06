package utils

import (
	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

const encoding = "cl100k_base"

func TokenizeInputText(text string) ([]int, error) {
	// if you don't want download dictionary at runtime, you can use offline loader
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	tke, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}

	// encode
	token := tke.Encode(text, nil, nil)
	return token, nil
}
