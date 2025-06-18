# AIBrix Embeddings API Guide

This guide provides comprehensive documentation for using the `/v1/embeddings` endpoint in AIBrix.

## Overview

The `/v1/embeddings` endpoint enables you to generate vector embeddings from text inputs using various embedding models. This endpoint follows the OpenAI embeddings API specification, ensuring compatibility with existing tools and libraries.

## Table of Contents

- [Prerequisites](#prerequisites)
- [API Reference](#api-reference)
- [Usage Examples](#usage-examples)
- [Configuration](#configuration)
- [Error Handling](#error-handling)
- [Performance Considerations](#performance-considerations)

## Prerequisites

### vLLM Configuration

To use embeddings with AIBrix, you need:

1. **vLLM version 0.4.3 or higher**
2. **Models loaded with embedding task support**

```bash
# Start vLLM with embedding support
python -m vllm.entrypoints.openai.api_server \
    --model sentence-transformers/all-MiniLM-L6-v2 \
    --task embed \
    --port 8000
```

### Supported Models

Common embedding models that work with vLLM:
- `sentence-transformers/all-MiniLM-L6-v2`
- `sentence-transformers/all-mpnet-base-v2`
- `intfloat/e5-large-v2`
- `BAAI/bge-large-en-v1.5`

## API Reference

### Request Format

```http
POST /v1/embeddings
Content-Type: application/json

{
  "input": "string | string[] | number[] | number[][]",
  "model": "string",
  "encoding_format": "float | base64",  // optional, default: "float"
  "dimensions": "number",               // optional
  "user": "string"                      // optional
}
```

### Response Format

```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "embedding": [0.1, 0.2, 0.3, ...],
      "index": 0
    }
  ],
  "model": "sentence-transformers/all-MiniLM-L6-v2",
  "usage": {
    "prompt_tokens": 8,
    "total_tokens": 8
  }
}
```

### Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `input` | `string \| string[] \| number[] \| number[][]` | Yes | Text input(s) to generate embeddings for |
| `model` | `string` | Yes | ID of the model to use |
| `encoding_format` | `string` | No | Format to return embeddings in: `"float"` or `"base64"` |
| `dimensions` | `integer` | No | Number of dimensions for the embedding (model-dependent) |
| `user` | `string` | No | Unique identifier for the user |

## Usage Examples

### Single Text Input

```python
import httpx

async def get_embedding():
    async with httpx.AsyncClient() as client:
        response = await client.post(
            "http://localhost:8080/v1/embeddings",
            json={
                "input": "The quick brown fox jumps over the lazy dog",
                "model": "sentence-transformers/all-MiniLM-L6-v2"
            }
        )
        return response.json()

# Example response:
# {
#   "object": "list",
#   "data": [
#     {
#       "object": "embedding",
#       "embedding": [0.012, -0.045, 0.123, ...],
#       "index": 0
#     }
#   ],
#   "model": "sentence-transformers/all-MiniLM-L6-v2",
#   "usage": {
#     "prompt_tokens": 9,
#     "total_tokens": 9
#   }
# }
```

### Batch Text Inputs

```python
async def get_batch_embeddings():
    async with httpx.AsyncClient() as client:
        response = await client.post(
            "http://localhost:8080/v1/embeddings",
            json={
                "input": [
                    "Hello world",
                    "How are you?",
                    "Goodbye!"
                ],
                "model": "sentence-transformers/all-MiniLM-L6-v2"
            }
        )
        return response.json()

# Example response:
# {
#   "object": "list",
#   "data": [
#     {
#       "object": "embedding",
#       "embedding": [0.012, -0.045, ...],
#       "index": 0
#     },
#     {
#       "object": "embedding", 
#       "embedding": [0.034, -0.067, ...],
#       "index": 1
#     },
#     {
#       "object": "embedding",
#       "embedding": [0.056, -0.089, ...],
#       "index": 2
#     }
#   ],
#   "model": "sentence-transformers/all-MiniLM-L6-v2",
#   "usage": {
#     "prompt_tokens": 6,
#     "total_tokens": 6
#   }
# }
```

### Base64 Encoding

```python
async def get_base64_embeddings():
    async with httpx.AsyncClient() as client:
        response = await client.post(
            "http://localhost:8080/v1/embeddings",
            json={
                "input": "Convert this to base64",
                "model": "sentence-transformers/all-MiniLM-L6-v2",
                "encoding_format": "base64"
            }
        )
        return response.json()

# The embedding will be returned as a base64-encoded string
```

### Token Array Input

```python
async def get_token_embeddings():
    async with httpx.AsyncClient() as client:
        response = await client.post(
            "http://localhost:8080/v1/embeddings",
            json={
                "input": [101, 7592, 2088, 102],  # Tokenized input
                "model": "sentence-transformers/all-MiniLM-L6-v2"
            }
        )
        return response.json()
```

### Using with OpenAI Client

```python
from openai import OpenAI

# Configure client to use AIBrix
client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed"  # AIBrix doesn't require API key by default
)

# Generate embeddings
response = client.embeddings.create(
    input="Your text here",
    model="sentence-transformers/all-MiniLM-L6-v2"
)

embedding = response.data[0].embedding
print(f"Embedding dimension: {len(embedding)}")
```

## Configuration

### Environment Variables

Configure AIBrix for embeddings support:

```bash
# Set the inference engine
export INFERENCE_ENGINE=vllm
export INFERENCE_ENGINE_VERSION=0.6.1
export INFERENCE_ENGINE_ENDPOINT=http://localhost:8000

# Optional: Set routing strategy
export ROUTING_ALGORITHM=random
```

### Model Configuration

Ensure your embedding model is properly configured in vLLM:

```bash
# Example vLLM startup with specific parameters
python -m vllm.entrypoints.openai.api_server \
    --model sentence-transformers/all-MiniLM-L6-v2 \
    --task embed \
    --port 8000 \
    --max-model-len 512 \
    --trust-remote-code
```

## Error Handling

### Common Error Responses

#### Model Not Found (404)
```json
{
  "object": "error",
  "message": "Model 'non-existent-model' not found",
  "type": "NotFoundError",
  "code": 404
}
```

#### Invalid Input Format (400)
```json
{
  "object": "error",
  "message": "Invalid input format",
  "type": "BadRequestError", 
  "code": 400
}
```

#### Model Doesn't Support Embeddings (501)
```json
{
  "object": "error",
  "message": "Inference engine vllm with version 0.6.1 not support embeddings",
  "type": "NotImplementedError",
  "code": 501
}
```

### Error Handling in Code

```python
async def safe_get_embeddings(text: str, model: str):
    try:
        async with httpx.AsyncClient() as client:
            response = await client.post(
                "http://localhost:8080/v1/embeddings",
                json={"input": text, "model": model}
            )
            
            if response.status_code == 200:
                return response.json()
            else:
                error_data = response.json()
                print(f"Error {response.status_code}: {error_data['message']}")
                return None
                
    except httpx.RequestError as e:
        print(f"Network error: {e}")
        return None
```

## Performance Considerations

### Batch Processing

For better performance, batch multiple inputs together:

```python
# Instead of making multiple single requests
texts = ["text1", "text2", "text3", "text4", "text5"]

# Batch them together
response = await client.post(
    "/v1/embeddings",
    json={
        "input": texts,  # Send all at once
        "model": "your-model"
    }
)
```

### Optimal Batch Sizes

- **Small models**: 50-100 texts per batch
- **Large models**: 10-20 texts per batch
- **Very large models**: 1-5 texts per batch

Monitor memory usage and adjust accordingly.

### Caching

Consider caching embeddings for frequently used texts:

```python
import hashlib
from typing import Dict, List

class EmbeddingCache:
    def __init__(self):
        self.cache: Dict[str, List[float]] = {}
    
    def get_cache_key(self, text: str, model: str) -> str:
        return hashlib.md5(f"{text}:{model}".encode()).hexdigest()
    
    async def get_embedding(self, text: str, model: str) -> List[float]:
        cache_key = self.get_cache_key(text, model)
        
        if cache_key in self.cache:
            return self.cache[cache_key]
        
        # Get embedding from API
        response = await self.fetch_embedding(text, model)
        embedding = response["data"][0]["embedding"]
        
        # Cache the result
        self.cache[cache_key] = embedding
        return embedding
```

## RAG Integration Example

Here's how to use embeddings in a Retrieval-Augmented Generation (RAG) system:

```python
import numpy as np
from typing import List, Tuple

class SimpleRAG:
    def __init__(self, embedding_model: str):
        self.embedding_model = embedding_model
        self.documents: List[str] = []
        self.embeddings: List[List[float]] = []
    
    async def add_document(self, text: str):
        """Add a document to the knowledge base."""
        # Get embedding for the document
        response = await self.get_embedding(text)
        embedding = response["data"][0]["embedding"]
        
        self.documents.append(text)
        self.embeddings.append(embedding)
    
    async def search(self, query: str, top_k: int = 3) -> List[Tuple[str, float]]:
        """Search for relevant documents."""
        # Get query embedding
        response = await self.get_embedding(query)
        query_embedding = np.array(response["data"][0]["embedding"])
        
        # Calculate similarities
        similarities = []
        for i, doc_embedding in enumerate(self.embeddings):
            similarity = np.dot(query_embedding, doc_embedding)
            similarities.append((self.documents[i], similarity))
        
        # Return top-k most similar documents
        similarities.sort(key=lambda x: x[1], reverse=True)
        return similarities[:top_k]
    
    async def get_embedding(self, text: str):
        async with httpx.AsyncClient() as client:
            response = await client.post(
                "http://localhost:8080/v1/embeddings",
                json={
                    "input": text,
                    "model": self.embedding_model
                }
            )
            return response.json()

# Usage
rag = SimpleRAG("sentence-transformers/all-MiniLM-L6-v2")

# Add documents
await rag.add_document("The capital of France is Paris.")
await rag.add_document("Python is a programming language.")
await rag.add_document("Machine learning is a subset of AI.")

# Search
results = await rag.search("What is the capital of France?")
print(results[0][0])  # Should return the document about Paris
```

## Troubleshooting

### Common Issues

1. **"Model not support embeddings"**
   - Ensure vLLM is started with `--task embed` flag
   - Verify the model supports embedding generation

2. **"Connection refused"**
   - Check that vLLM server is running on the specified port
   - Verify `INFERENCE_ENGINE_ENDPOINT` environment variable

3. **Out of memory errors**
   - Reduce batch size
   - Use a smaller model
   - Increase GPU memory allocation

4. **Slow performance**
   - Use GPU acceleration if available
   - Implement request batching
   - Consider model quantization

### Debugging

Enable debug logging for more detailed error information:

```python
import logging

# Enable debug logging
logging.basicConfig(level=logging.DEBUG)

# Your embedding requests will now show detailed logs
```

## Next Steps

- Explore different embedding models for your use case
- Implement caching for production deployments
- Set up monitoring and metrics collection
- Consider implementing custom preprocessing for your domain

For more information, see the [AIBrix documentation](https://aibrix.readthedocs.io/) and [vLLM embedding guide](https://docs.vllm.ai/).