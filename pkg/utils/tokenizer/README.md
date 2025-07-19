# Tokenizer Package

The tokenizer package provides a unified interface for text tokenization in AIBrix, supporting both local and remote tokenization implementations. It's designed to work with various LLM inference engines like vLLM, SGLang, and others.

## Overview

This package implements a flexible tokenizer architecture that:
- Provides a common interface for different tokenization backends
- Supports local tokenizers (tiktoken, character-based)
- Supports remote tokenizers via HTTP API (vLLM, SGLang, etc.)
- Maintains backward compatibility with legacy APIs
- Follows Go idioms with a flattened package structure

## Architecture

### Core Components

1. **Interfaces** (`interfaces.go`)
   - `Tokenizer`: Basic tokenization interface
   - `ExtendedTokenizer`: Advanced features including detokenization
   - `RemoteTokenizer`: Remote tokenizer with health checks
   - `EngineAdapter`: Handles engine-specific differences
   - `TokenizerV2`: Deprecated alias for ExtendedTokenizer

2. **Types** (`types.go`)
   - Common data structures for all tokenizers
   - Request/response types for different engines
   - Configuration structures

3. **Implementations**
   - **Local Tokenizers**:
     - `local_tiktoken.go`: OpenAI tiktoken implementation
     - `local_characters.go`: Simple character-based tokenizer
   - **Remote Tokenizer**:
     - `remote_tokenizer.go`: Generic remote tokenizer
     - `remote_client.go`: HTTP client with retry logic
   - **Engine Adapters**:
     - `adapter_vllm.go`: vLLM-specific implementation
     - `adapter_sglang.go`: SGLang adapter (limited support)
     - `adapter_factory.go`: Adapter creation logic

## Quick Start

### Basic Usage

```go
import "github.com/vllm-project/aibrix/pkg/utils/tokenizer"

// Create a local tiktoken tokenizer
tok1 := tokenizer.NewTiktokenTokenizer()
tokens, err := tok1.TokenizeInputText("Hello, world!")

// Create a character tokenizer
tok2 := tokenizer.NewCharacterTokenizer()
tokens, err := tok2.TokenizeInputText("Hello, world!")

// Create a remote vLLM tokenizer (backward compatible)
config := tokenizer.VLLMTokenizerConfig{
    BaseURL: "http://vllm-service:8000",
    Model: "meta-llama/Llama-2-7b-hf",
    Timeout: 30,
}
tok3, err := tokenizer.NewVLLMTokenizer(config)
tokens, err := tok3.TokenizeInputText("Hello, world!")
```

### Advanced Usage with Remote Tokenizer

```go
import (
    "context"
    "time"
    "github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// Create a remote tokenizer with full configuration
config := tokenizer.RemoteTokenizerConfig{
    Engine:             "vllm",
    Endpoint:           "http://vllm-service:8000",
    Model:              "meta-llama/Llama-2-7b-hf",
    Timeout:            30 * time.Second,
    MaxRetries:         3,
    AddSpecialTokens:   true,
    ReturnTokenStrings: true,
}

tok, err := tokenizer.NewRemoteTokenizer(config)
if err != nil {
    log.Fatal(err)
}

// Use extended features
ctx := context.Background()

// Tokenize with options
input := tokenizer.TokenizeInput{
    Type:                tokenizer.CompletionInput,
    Text:                "Hello, world!",
    AddSpecialTokens:    true,
    ReturnTokenStrings:  true,
}
result, err := tok.TokenizeWithOptions(ctx, input)
if err != nil {
    log.Fatal(err)
}

fmt.Printf("Token count: %d\n", result.Count)
fmt.Printf("Tokens: %v\n", result.Tokens)
fmt.Printf("Token strings: %v\n", result.TokenStrings)

// Detokenize
text, err := tok.Detokenize(ctx, result.Tokens)
fmt.Printf("Detokenized: %s\n", text)

// Check health
if tok.IsHealthy(ctx) {
    fmt.Println("Tokenizer service is healthy")
}
```

### Chat Tokenization

```go
// Tokenize chat messages
chatInput := tokenizer.TokenizeInput{
    Type: tokenizer.ChatInput,
    Messages: []tokenizer.ChatMessage{
        {Role: "system", Content: "You are a helpful assistant."},
        {Role: "user", Content: "What is the weather today?"},
    },
    AddGenerationPrompt: true,
}

result, err := tok.TokenizeWithOptions(ctx, chatInput)
```

## File Structure

```
tokenizer/
├── Core Components
│   ├── interfaces.go          # Interface definitions
│   ├── types.go              # Type definitions
│   ├── errors.go             # Error types
│   ├── factory.go            # Factory methods
│   └── utils.go              # Shared utilities
│
├── Local Implementations
│   ├── local_tiktoken.go     # Tiktoken tokenizer
│   └── local_characters.go   # Character tokenizer
│
├── Remote Implementation
│   ├── remote_tokenizer.go   # Generic remote tokenizer
│   └── remote_client.go      # HTTP client
│
├── Engine Adapters
│   ├── adapter_factory.go    # Adapter factory
│   ├── adapter_vllm.go       # vLLM adapter
│   └── adapter_sglang.go     # SGLang adapter
│
├── Compatibility
│   ├── compat_vllm.go        # Legacy vLLM tokenizer
│   └── tokenizer.go          # Legacy factory function
│
└── Tests
    ├── vllm_tokenizer_test.go
    └── factory_test.go
```

## Supported Tokenizers

### Local Tokenizers

1. **Tiktoken** (`tiktoken`)
   - Uses OpenAI's tiktoken library
   - Encoding: `cl100k_base`
   - Suitable for GPT models

2. **Character** (`character`)
   - Simple byte-based tokenization
   - Useful for testing and special cases

### Remote Tokenizers

1. **vLLM** (`vllm`)
   - Full support for tokenization and detokenization
   - Chat template support
   - Special tokens handling
   - Multimodal support (via `mm_processor_kwargs`)

2. **SGLang** (`sglang`)
   - Limited support (no tokenize/detokenize endpoints)
   - As of 2025-07-18, SGLang Runtime's HTTP service does not expose `/tokenize` or `/detokenize` endpoints
   - Official documentation only lists endpoints like `/generate`, `/encode`, `/v1/chat/completions`, `/v1/embeddings`, etc.
   - Community has requested tokenizer endpoints (Issues #5653 and #7068) for RL/Agent frameworks, but these remain in discussion/planning phase
   - **Workarounds if you need tokenization with SGLang**:
     - **Client-side tokenization**: Use HuggingFace Tokenizer or similar tools to convert text to `input_ids` before calling SGLang endpoints
     - **Skip server tokenization**: Start SGLang with `--skip-tokenizer-init` flag and pass `input_ids` arrays directly in requests
   - The adapter is included as a placeholder for future implementation when/if SGLang adds tokenizer endpoints

## Extending the Package

### Adding a New Engine Adapter

1. Create a new adapter file (e.g., `adapter_myengine.go`):

```go
package tokenizer

type MyEngineAdapter struct {
    model string
}

func NewMyEngineAdapter(model string) *MyEngineAdapter {
    return &MyEngineAdapter{model: model}
}

func (a *MyEngineAdapter) GetTokenizePath() string {
    return "/my/tokenize/path"
}

func (a *MyEngineAdapter) SupportsTokenization() bool {
    return true
}

func (a *MyEngineAdapter) PrepareTokenizeRequest(input TokenizeInput) (interface{}, error) {
    // Convert TokenizeInput to engine-specific request
}

func (a *MyEngineAdapter) ParseTokenizeResponse(data []byte) (*TokenizeResult, error) {
    // Parse engine-specific response to TokenizeResult
}

// ... implement other EngineAdapter methods
```

2. Register in adapter factory (`adapter_factory.go`):

```go
func NewEngineAdapter(engine string, model string) (EngineAdapter, error) {
    switch engine {
    // ... existing cases
    case "myengine":
        return NewMyEngineAdapter(model), nil
    default:
        return nil, fmt.Errorf("unsupported engine: %s", engine)
    }
}
```

### Adding a New Local Tokenizer

1. Create a new tokenizer file (e.g., `local_custom.go`):

```go
package tokenizer

type CustomTokenizer struct {
    // fields
}

func (t *CustomTokenizer) TokenizeInputText(text string) ([]byte, error) {
    // Implementation
    tokens := tokenizeCustom(text) // Your tokenization logic
    return intToByteArray(tokens), nil
}
```

2. Add factory method in `factory.go`:

```go
func NewCustomTokenizer() Tokenizer {
    return &CustomTokenizer{}
}
```

## API Reference

### Core Interfaces

#### Tokenizer
```go
type Tokenizer interface {
    TokenizeInputText(string) ([]byte, error)
}
```

#### ExtendedTokenizer
```go
type ExtendedTokenizer interface {
    Tokenizer
    TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error)
    Detokenize(ctx context.Context, tokens []int) (string, error)
}
```

### Key Types

#### TokenizeInput
```go
type TokenizeInput struct {
    Type                TokenizeInputType     // "completion" or "chat"
    Text                string                // For completion input
    Messages            []ChatMessage         // For chat input
    AddSpecialTokens    bool                 // Add special tokens
    ReturnTokenStrings  bool                 // Return token strings
    AddGenerationPrompt bool                 // For chat only
}
```

#### TokenizeResult
```go
type TokenizeResult struct {
    Count        int      // Number of tokens
    MaxModelLen  int      // Maximum model length
    Tokens       []int    // Token IDs
    TokenStrings []string // Token strings (optional)
}
```

## Configuration

### RemoteTokenizerConfig
```go
type RemoteTokenizerConfig struct {
    Engine             string        // "vllm", "sglang", etc.
    Endpoint           string        // Base URL
    Model              string        // Model name (optional)
    Timeout            time.Duration // Request timeout
    MaxRetries         int          // Retry attempts
    AddSpecialTokens   bool         // Default: true
    ReturnTokenStrings bool         // Default: false
}
```

### VLLMTokenizerConfig (Legacy)
```go
type VLLMTokenizerConfig struct {
    BaseURL            string // vLLM server URL
    Model              string // Model name
    Timeout            int    // Timeout in seconds
    MaxRetries         int    // Retry attempts
    AddSpecialTokens   bool   // Add special tokens
    ReturnTokenStrings bool   // Return token strings
}
```

## Error Handling

The package defines several error types for better error handling:

- `ErrInvalidConfig`: Configuration validation errors
- `ErrTokenizationFailed`: Tokenization failures
- `ErrDetokenizationFailed`: Detokenization failures
- `ErrUnsupportedOperation`: Operation not supported by engine
- `ErrHTTPRequest`: HTTP request errors

Example:
```go
tokens, err := tokenizer.TokenizeInputText("text")
if err != nil {
    switch e := err.(type) {
    case tokenizer.ErrUnsupportedOperation:
        log.Printf("Operation %s not supported by %s", e.Operation, e.Engine)
    case tokenizer.ErrHTTPRequest:
        log.Printf("HTTP error %d: %s", e.StatusCode, e.Message)
    default:
        log.Printf("Tokenization error: %v", err)
    }
}
```

## Performance Considerations

1. **Connection Pooling**: The HTTP client uses connection pooling for better performance
2. **Retry Logic**: Automatic retry with exponential backoff for transient failures
3. **Timeout Configuration**: Configure appropriate timeouts based on your use case
4. **Local vs Remote**: Local tokenizers are faster but may not match model's exact tokenization

## Testing

Run tests with:
```bash
go test ./pkg/utils/tokenizer/...
```

The package includes comprehensive tests for:
- Basic tokenization functionality
- Remote tokenizer with mock servers
- Error handling scenarios
- Backward compatibility

## Migration Guide

### From Legacy VLLMTokenizer to RemoteTokenizer

```go
// Old code
config := tokenizer.VLLMTokenizerConfig{
    BaseURL: "http://vllm:8000",
    Model: "llama2",
}
tok, _ := tokenizer.NewVLLMTokenizer(config)

// New code (recommended)
config := tokenizer.RemoteTokenizerConfig{
    Engine: "vllm",
    Endpoint: "http://vllm:8000",
    Model: "llama2",
    Timeout: 30 * time.Second,
}
tok, _ := tokenizer.NewRemoteTokenizer(config)
```

The legacy API remains fully supported for backward compatibility.

## Contributing

When contributing to this package:

1. Maintain backward compatibility
2. Add tests for new features
3. Update this README for significant changes
4. Follow Go idioms and conventions
5. Ensure no circular dependencies

## License

This package is part of the AIBrix project and follows the same Apache 2.0 license.