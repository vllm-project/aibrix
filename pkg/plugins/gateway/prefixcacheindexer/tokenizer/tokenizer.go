package tokenizer

type PrefixCacheIndexer interface {
	TokenizeInputText(string) []byte
}
