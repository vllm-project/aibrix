# Tokenizer Package

The tokenizer package provides a unified interface for text tokenization in AIBrix, supporting both local and remote tokenization implementations. It's designed to work with various LLM inference engines like vLLM, SGLang, and others.

## Overview

This package implements a flexible tokenizer architecture that:
- Provides a common interface for different tokenization backends
- Supports local tokenizers (tiktoken, character-based)
- Supports remote tokenizers via HTTP API (vLLM, SGLang, etc.)
- Follows Go idioms with a minimal public API surface
- Uses type assertions for advanced features (progressive disclosure pattern)

## Architecture

### Core Design Principles

1. **Minimal Public API**: Only the `Tokenizer` interface is exported
2. **Progressive Disclosure**: Basic usage is simple, advanced features available via type assertions
3. **Type Safety**: Compile-time checks with Go's type system
4. **Extensibility**: Easy to add new tokenizer implementations

### Interface Hierarchy

```
Tokenizer (public interface)
    ├── Local implementations
    │   ├── TiktokenTokenizer
    │   └── CharacterTokenizer
    └── Remote implementation
        └── remoteTokenizerImpl (supports advanced features internally)
```

## Quick Start

### Basic Usage

```go
import "github.com/vllm-project/aibrix/pkg/utils/tokenizer"

// Option 1: Using the factory function
tok1, err := tokenizer.NewTokenizer("tiktoken", nil)
tokens, err := tok1.TokenizeInputText("Hello, world!")

// Option 2: Using direct constructors
// Create a local tiktoken tokenizer
tok2 := tokenizer.NewTiktokenTokenizer()
tokens, err := tok2.TokenizeInputText("Hello, world!")

// Create a character tokenizer
tok3 := tokenizer.NewCharacterTokenizer()
tokens, err := tok3.TokenizeInputText("Hello, world!")

// Create a remote tokenizer
config := tokenizer.RemoteTokenizerConfig{
    Engine:   "vllm",
    Endpoint: "http://vllm-service:8000",
    Model:    "meta-llama/Llama-2-7b-hf",
    Timeout:  30 * time.Second,
}
tok4, err := tokenizer.NewRemoteTokenizer(config)
tokens, err := tok4.TokenizeInputText("Hello, world!")

// Using factory function for remote tokenizer
tok5, err := tokenizer.NewTokenizer("remote", config)
tokens, err := tok5.TokenizeInputText("Hello, world!")
```

### Advanced Features with Type Assertions

Since advanced interfaces are not exported, use type assertions to access extended functionality:

```go
import (
    "context"
    "time"
    "github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// Create a remote tokenizer
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

// Basic tokenization (always available)
tokens, err := tok.TokenizeInputText("Hello, world!")

// Advanced tokenization features
if extended, ok := tok.(interface {
    TokenizeWithOptions(context.Context, tokenizer.TokenizeInput) (*tokenizer.TokenizeResult, error)
    Detokenize(context.Context, []int) (string, error)
}); ok {
    ctx := context.Background()
    
    // Tokenize with options
    input := tokenizer.TokenizeInput{
        Type:                tokenizer.CompletionInput,
        Text:                "Hello, world!",
        AddSpecialTokens:    true,
        ReturnTokenStrings:  true,
    }
    result, err := extended.TokenizeWithOptions(ctx, input)
    if err != nil {
        log.Fatal(err)
    }
    
    fmt.Printf("Token count: %d\n", result.Count)
    fmt.Printf("Tokens: %v\n", result.Tokens)
    fmt.Printf("Token strings: %v\n", result.TokenStrings)
    
    // Detokenize
    text, err := extended.Detokenize(ctx, result.Tokens)
    fmt.Printf("Detokenized: %s\n", text)
}

// Remote-specific features
if remote, ok := tok.(interface {
    IsHealthy(context.Context) bool
    GetEndpoint() string
    Close() error
}); ok {
    ctx := context.Background()
    
    // Check health
    if remote.IsHealthy(ctx) {
        fmt.Println("Tokenizer service is healthy")
    }
    
    // Get endpoint
    fmt.Printf("Using endpoint: %s\n", remote.GetEndpoint())
    
    // Clean up
    defer remote.Close()
}
```

### Chat Tokenization

```go
// Chat tokenization requires type assertion to access advanced features
if extended, ok := tok.(interface {
    TokenizeWithOptions(context.Context, tokenizer.TokenizeInput) (*tokenizer.TokenizeResult, error)
}); ok {
    ctx := context.Background()
    
    chatInput := tokenizer.TokenizeInput{
        Type: tokenizer.ChatInput,
        Messages: []tokenizer.ChatMessage{
            {Role: "system", Content: "You are a helpful assistant."},
            {Role: "user", Content: "What is the weather today?"},
        },
        AddGenerationPrompt: true,
    }
    
    result, err := extended.TokenizeWithOptions(ctx, chatInput)
    if err != nil {
        log.Fatal(err)
    }
}
```

## File Structure

```
tokenizer/
├── Core Components
│   ├── interfaces.go          # Interface definitions (only Tokenizer exported)
│   ├── types.go              # Type definitions
│   ├── errors.go             # Error types
│   ├── utils.go              # Shared utilities
│   └── tokenizer.go          # Main factory function
│
├── Local Implementations
│   ├── local_tiktoken.go     # Tiktoken tokenizer with constructor
│   └── local_characters.go   # Character tokenizer with constructor
│
├── Remote Implementation
│   ├── remote_tokenizer.go   # Generic remote tokenizer
│   └── remote_client.go      # HTTP client with retry logic
│
├── Engine Adapters
│   ├── adapter_vllm.go       # vLLM adapter (internal)
│   └── adapter_sglang.go     # SGLang adapter (internal)
│
└── Tests
    └── remote_client_test.go
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

The package currently supports the following remote tokenizer engines:

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

func NewCustomTokenizer() Tokenizer {
    return &CustomTokenizer{}
}
```

2. The tokenizer automatically implements the `Tokenizer` interface.

## API Reference

### Public Interface

#### Tokenizer
The only exported interface, providing basic tokenization:

```go
type Tokenizer interface {
    TokenizeInputText(string) ([]byte, error)
}
```

#### Factory Function
The main factory function for creating tokenizer instances:

```go
func NewTokenizer(tokenizerType string, config interface{}) (Tokenizer, error)
```

Supported tokenizer types:
- `"tiktoken"` - OpenAI's tiktoken tokenizer (config: nil)
- `"character"` - Simple character-based tokenizer (config: nil) 
- `"remote"` - Remote tokenizer via HTTP API (config: RemoteTokenizerConfig)

### Advanced Features via Type Assertions

While not exported, advanced features are available through type assertions:

```go
// Extended tokenization features
interface {
    TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error)
    Detokenize(ctx context.Context, tokens []int) (string, error)
}

// Remote-specific features
interface {
    IsHealthy(ctx context.Context) bool
    GetEndpoint() string
    Close() error
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

#### ChatMessage
```go
type ChatMessage struct {
    Role    string // "system", "user", "assistant"
    Content string // Message content
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
2. **Retry Logic**: 
   - Automatic retry with exponential backoff for transient failures
   - Respects `Retry-After` header for rate-limited requests (HTTP 429)
   - Configurable retry attempts with smart status code handling
   - Only retries on specific status codes (408, 429, 500, 502, 503, 504)
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
- Type assertion patterns

## Migration Guide

### From Deprecated Components

1. **VLLMTokenizer to RemoteTokenizer**:
```go
// Old (no longer supported)
// config := tokenizer.VLLMTokenizerConfig{...}
// tok, _ := tokenizer.NewVLLMTokenizer(config)

// New
config := tokenizer.RemoteTokenizerConfig{
    Engine:   "vllm",
    Endpoint: "http://vllm:8000",
    Model:    "llama2",
    Timeout:  30 * time.Second,
}
tok, _ := tokenizer.NewRemoteTokenizer(config)
```

2. **ExtendedTokenizer/RemoteTokenizer interfaces**:
```go
// Old (no longer exported)
// var tok tokenizer.ExtendedTokenizer

// New - use type assertions
tok, _ := tokenizer.NewRemoteTokenizer(config)
if extended, ok := tok.(interface {
    TokenizeWithOptions(context.Context, tokenizer.TokenizeInput) (*tokenizer.TokenizeResult, error)
}); ok {
    // Use extended features
}
```

## Contributing

When contributing to this package:

1. Maintain the minimal public API philosophy
2. Add tests for new features
3. Update this README for significant changes
4. Follow Go idioms and conventions
5. Use type assertions for advanced features
6. Ensure backward compatibility for the `Tokenizer` interface

## License

This package is part of the AIBrix project and follows the same Apache 2.0 license.