package tokenizer

type Tokenizer interface {
	TokenizeInputText(string) ([]byte, error)
}
