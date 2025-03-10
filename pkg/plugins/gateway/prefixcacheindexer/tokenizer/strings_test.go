package tokenizer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_StringTokenizer(t *testing.T) {
	sTokenizer := NewStringTokenizer()

	bArr, err := sTokenizer.TokenizeInputText("this is first message")
	assert.NoError(t, err)
	fmt.Println(bArr)

	// assert.Equal(t, 1, 0)
}
