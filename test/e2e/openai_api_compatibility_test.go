/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"io"
	"testing"

	"github.com/openai/openai-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompletionStreaming(t *testing.T) {
	t.Skip("TODO(@jeffwan): fix me")
	client := createOpenAIClient(gatewayURL, apiKey)

	stream := client.Completions.NewStreaming(context.TODO(), openai.CompletionNewParams{
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String("Say this is a test"),
		},
		Model: modelName,
	})

	var fullText string
	chunkCount := 0

	for stream.Next() {
		chunk := stream.Current()
		chunkCount++

		assert.Equal(t, modelName, chunk.Model, "model should match in streaming chunk")
		assert.NotEmpty(t, chunk.ID, "chunk should have an ID")
		assert.Equal(t, "text_completion", string(chunk.Object), "chunk object type should be text_completion")

		if len(chunk.Choices) > 0 && chunk.Choices[0].Text != "" {
			fullText += chunk.Choices[0].Text
			t.Logf("Received chunk %d: %q", chunkCount, chunk.Choices[0].Text)
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
			t.Logf("Stream finished with reason: %s", chunk.Choices[0].FinishReason)
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("streaming failed: %v", err)
	}

	assert.Greater(t, chunkCount, 0, "should receive at least one chunk")
	assert.NotEmpty(t, fullText, "should receive some text content")
	t.Logf("Received %d chunks with full text: %q", chunkCount, fullText)
}

func TestChatCompletionStreaming(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	stream := client.Chat.Completions.NewStreaming(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		},
		Model: modelName,
	})

	var fullText string
	chunkCount := 0

	for stream.Next() {
		chunk := stream.Current()
		chunkCount++

		assert.Equal(t, modelName, chunk.Model, "model should match in streaming chunk")
		assert.NotEmpty(t, chunk.ID, "chunk should have an ID")
		assert.Equal(t, "chat.completion.chunk", string(chunk.Object), "chunk object type should be chat.completion.chunk")

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			fullText += chunk.Choices[0].Delta.Content
			t.Logf("Received chunk %d: %q", chunkCount, chunk.Choices[0].Delta.Content)
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
			t.Logf("Stream finished with reason: %s", chunk.Choices[0].FinishReason)
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("streaming failed: %v", err)
	}

	assert.Greater(t, chunkCount, 0, "should receive at least one chunk")
	assert.NotEmpty(t, fullText, "should receive some text content")
	t.Logf("Received %d chunks with full text: %q", chunkCount, fullText)
}

func TestChatCompletionStreamingMultiTurn(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("Hello, how are you?"),
	}

	for turn := 1; turn <= 3; turn++ {
		t.Logf("Turn %d: Sending message", turn)

		stream := client.Chat.Completions.NewStreaming(context.TODO(), openai.ChatCompletionNewParams{
			Messages: messages,
			Model:    modelName,
		})

		var fullText string
		chunkCount := 0

		for stream.Next() {
			chunk := stream.Current()
			chunkCount++

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				fullText += chunk.Choices[0].Delta.Content
			}
		}

		require.NoError(t, stream.Err(), "streaming failed on turn %d", turn)
		assert.Greater(t, chunkCount, 0, "should receive chunks on turn %d", turn)
		assert.NotEmpty(t, fullText, "should receive text on turn %d", turn)

		t.Logf("Turn %d: Received %d chunks, response: %q", turn, chunkCount, fullText)

		messages = append(messages, openai.AssistantMessage(fullText))
		messages = append(messages, openai.UserMessage("Tell me more"))
	}
}

func TestCompletionStreamingError(t *testing.T) {
	client := createOpenAIClient(gatewayURL, "invalid-api-key")

	stream := client.Completions.NewStreaming(context.TODO(), openai.CompletionNewParams{
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String("Say this is a test"),
		},
		Model: modelName,
	})

	hasError := false
	for stream.Next() {
		// Should not receive any chunks
		t.Logf("Unexpectedly received chunk: %+v", stream.Current())
	}

	if err := stream.Err(); err != nil {
		hasError = true
		t.Logf("Expected error received: %v", err)
	}

	assert.True(t, hasError, "should receive authentication error")
}

func TestChatCompletionStreamingError(t *testing.T) {
	client := createOpenAIClient(gatewayURL, "invalid-api-key")

	stream := client.Chat.Completions.NewStreaming(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		},
		Model: modelName,
	})

	hasError := false
	for stream.Next() {
		// Should not receive any chunks
		t.Logf("Unexpectedly received chunk: %+v", stream.Current())
	}

	if err := stream.Err(); err != nil {
		hasError = true
		t.Logf("Expected error received: %v", err)
	}

	assert.True(t, hasError, "should receive authentication error")
}

func TestCompletionStreamingCancellation(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := client.Completions.NewStreaming(ctx, openai.CompletionNewParams{
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String("Say this is a test"),
		},
		Model: modelName,
	})

	chunkCount := 0
	for stream.Next() {
		chunkCount++
		t.Logf("Received chunk %d", chunkCount)

		// Cancel after first chunk
		if chunkCount == 1 {
			cancel()
		}
	}

	err := stream.Err()
	if err != nil && err != io.EOF && err != context.Canceled {
		t.Logf("Stream error: %v", err)
	}

	assert.Greater(t, chunkCount, 0, "should receive at least one chunk before cancellation")
	t.Logf("Successfully cancelled after %d chunks", chunkCount)
}

func TestChatCompletionStreamingCancellation(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		},
		Model: modelName,
	})

	chunkCount := 0
	for stream.Next() {
		chunkCount++
		t.Logf("Received chunk %d", chunkCount)

		// Cancel after first chunk
		if chunkCount == 1 {
			cancel()
		}
	}

	err := stream.Err()
	if err != nil && err != io.EOF && err != context.Canceled {
		t.Logf("Stream error: %v", err)
	}

	assert.Greater(t, chunkCount, 0, "should receive at least one chunk before cancellation")
	t.Logf("Successfully cancelled after %d chunks", chunkCount)
}

// Test error response format compatibility
func TestOpenAIErrorFormat401(t *testing.T) {
	// Ensure model is accessible before testing authentication failure
	validateInference(t, modelName)

	client := createOpenAIClient(gatewayURL, "invalid-api-key")

	_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Test"),
		},
		Model: modelName,
	})

	require.Error(t, err)
	var apiErr *openai.Error
	if assert.ErrorAs(t, err, &apiErr) {
		assert.Equal(t, 401, apiErr.StatusCode, "Should return 401 status code")
		t.Logf("401 Error message: %s", apiErr.Message)
		// Mock returns different error messages, just verify we got a 401
		assert.NotEmpty(t, apiErr.Message, "Error message should not be empty")
	}
}

func TestOpenAIErrorFormat400MissingParams(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	// Test missing model parameter
	_, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Test"),
		},
		Model: "", // Empty model
	})

	require.Error(t, err)
	var apiErr *openai.Error
	if assert.ErrorAs(t, err, &apiErr) {
		assert.Equal(t, 400, apiErr.StatusCode, "Should return 400 for missing parameters")
		t.Logf("400 Error message: %s", apiErr.Message)
	}
}

// Test non-streaming responses match OpenAI format exactly
func TestCompletionsResponseFormat(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	completion, err := client.Completions.New(context.TODO(), openai.CompletionNewParams{
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String("Say hello"),
		},
		Model: modelName,
	})

	require.NoError(t, err)
	assert.Equal(t, "text_completion", string(completion.Object))
	assert.NotEmpty(t, completion.ID)
	assert.Greater(t, completion.Created, int64(0))
	assert.Equal(t, modelName, completion.Model)
	assert.NotEmpty(t, completion.Choices)
	assert.Greater(t, completion.Usage.PromptTokens, int64(0))
	assert.Greater(t, completion.Usage.CompletionTokens, int64(0))
	assert.Greater(t, completion.Usage.TotalTokens, int64(0))
}

func TestChatCompletionsResponseFormat(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say hello"),
		},
		Model: modelName,
	})

	require.NoError(t, err)
	assert.Equal(t, "chat.completion", string(chatCompletion.Object))
	assert.NotEmpty(t, chatCompletion.ID)
	assert.Greater(t, chatCompletion.Created, int64(0))
	assert.Equal(t, modelName, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices)
	assert.Equal(t, "assistant", string(chatCompletion.Choices[0].Message.Role))
	assert.NotEmpty(t, chatCompletion.Choices[0].Message.Content)
	assert.Greater(t, chatCompletion.Usage.PromptTokens, int64(0))
	assert.Greater(t, chatCompletion.Usage.CompletionTokens, int64(0))
	assert.Greater(t, chatCompletion.Usage.TotalTokens, int64(0))
}

// ============================================================================
// Embeddings API Tests
// ============================================================================

func TestEmbeddingsSingleInput(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	embedding, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("The quick brown fox jumps over the lazy dog"),
		},
		Model: openai.EmbeddingModel(modelName),
	})

	require.NoError(t, err, "embeddings request should succeed")
	assert.Equal(t, "list", string(embedding.Object))
	assert.NotEmpty(t, embedding.Data, "should have at least one embedding")
	assert.Equal(t, int64(0), embedding.Data[0].Index)
	assert.Equal(t, "embedding", string(embedding.Data[0].Object))
	assert.NotEmpty(t, embedding.Data[0].Embedding, "embedding vector should not be empty")
	assert.Equal(t, 1536, len(embedding.Data[0].Embedding), "mock should return 1536 dimensions")
	assert.Greater(t, embedding.Usage.PromptTokens, int64(0))
	assert.Greater(t, embedding.Usage.TotalTokens, int64(0))

	t.Logf("Generated embedding with %d dimensions, %d tokens used",
		len(embedding.Data[0].Embedding), embedding.Usage.TotalTokens)
}

func TestEmbeddingsMultipleInputs(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	inputs := []string{
		"Hello, world!",
		"How are you?",
		"This is a test.",
	}

	embedding, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: inputs,
		},
		Model: openai.EmbeddingModel(modelName),
	})

	require.NoError(t, err, "embeddings request should succeed")
	assert.Equal(t, "list", string(embedding.Object))
	assert.Len(t, embedding.Data, 3, "should have 3 embeddings")

	for i, embData := range embedding.Data {
		assert.Equal(t, int64(i), embData.Index, "index should match input order")
		assert.Equal(t, "embedding", string(embData.Object))
		assert.NotEmpty(t, embData.Embedding, "embedding vector should not be empty")
		assert.Equal(t, 1536, len(embData.Embedding), "mock should return 1536 dimensions")
		t.Logf("Embedding %d: %d dimensions", i, len(embData.Embedding))
	}

	assert.Greater(t, embedding.Usage.PromptTokens, int64(0))
	assert.Greater(t, embedding.Usage.TotalTokens, int64(0))
}

func TestEmbeddingsWithDimensions(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	customDimensions := 512

	embedding, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("Test with custom dimensions"),
		},
		Model:      openai.EmbeddingModel(modelName),
		Dimensions: openai.Int(int64(customDimensions)),
	})

	require.NoError(t, err, "embeddings request with custom dimensions should succeed")
	assert.NotEmpty(t, embedding.Data)
	assert.Equal(t, customDimensions, len(embedding.Data[0].Embedding),
		"mock should respect custom dimensions parameter")

	t.Logf("Generated embedding with custom %d dimensions", len(embedding.Data[0].Embedding))
}

func TestEmbeddingsLargeModel(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	embedding, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("Test with large embedding model"),
		},
		Model: openai.EmbeddingModel(modelName),
	})

	require.NoError(t, err, "embeddings request should succeed")
	assert.NotEmpty(t, embedding.Data)
	assert.Equal(t, 1536, len(embedding.Data[0].Embedding),
		"mock should return default 1536 dimensions")

	t.Logf("Generated embedding with %d dimensions", len(embedding.Data[0].Embedding))
}

func TestEmbeddingsError401(t *testing.T) {
	// Ensure model is accessible before testing authentication failure
	validateInference(t, modelName)

	client := createOpenAIClient(gatewayURL, "invalid-api-key")

	_, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("Test with invalid API key"),
		},
		Model: openai.EmbeddingModel(modelName),
	})

	require.Error(t, err)
	var apiErr *openai.Error
	if assert.ErrorAs(t, err, &apiErr) {
		assert.Equal(t, 401, apiErr.StatusCode, "should return 401 for invalid API key")
		t.Logf("401 Error: %s", apiErr.Message)
	}
}

func TestEmbeddingsError400MissingModel(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	_, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfString: openai.String("Test"),
		},
		Model: "",
	})

	require.Error(t, err)
	var apiErr *openai.Error
	if assert.ErrorAs(t, err, &apiErr) {
		assert.Equal(t, 400, apiErr.StatusCode, "should return 400 for missing model")
		t.Logf("400 Error: %s", apiErr.Message)
	}
}

func TestEmbeddingsBatchProcessing(t *testing.T) {
	client := createOpenAIClient(gatewayURL, apiKey)

	// Test batch processing with multiple inputs
	batchSize := 10
	inputs := make([]string, batchSize)
	for i := 0; i < batchSize; i++ {
		inputs[i] = "Batch input number " + string(rune(i+'0'))
	}

	embedding, err := client.Embeddings.New(context.TODO(), openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: inputs,
		},
		Model: openai.EmbeddingModel(modelName),
	})

	require.NoError(t, err, "batch embeddings should succeed")
	assert.Len(t, embedding.Data, batchSize, "should return embeddings for all inputs")

	// Verify indices are correct
	for i := 0; i < batchSize; i++ {
		assert.Equal(t, int64(i), embedding.Data[i].Index, "index should match input order")
		assert.NotEmpty(t, embedding.Data[i].Embedding, "each embedding should have a vector")
	}

	t.Logf("Successfully processed batch of %d embeddings", batchSize)
}
