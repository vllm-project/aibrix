package tokenizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_StringTokenizer(t *testing.T) {
	sTokenizer := NewStringTokenizer()

	bArr, err := sTokenizer.TokenizeInputText("this is first message")
	assert.NoError(t, err)
	assert.Equal(t, []byte{116, 104, 105, 115, 105, 115, 102, 105, 114, 115, 116, 109, 101, 115, 115, 97, 103, 101}, bArr)
}
