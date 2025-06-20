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

package gateway

type (
	OpenAiRequestType string
	OpenAiRequestPath string
)

var (
	OpenAiRequestChatCompletionsType OpenAiRequestType = "chat-completions"
	OpenAiRequestCompletionsType     OpenAiRequestType = "completions"
	OpenAiRequestEmbeddingsType      OpenAiRequestType = "embeddings"
	OpenAiRequestUnknownType         OpenAiRequestType = "unknown"
	OpenAiRequestChatCompletionsPath OpenAiRequestPath = "/v1/chat/completions"
	OpenAiRequestCompletionsPath     OpenAiRequestPath = "/v1/completions"
	OpenAiRequestEmbeddingsPath      OpenAiRequestPath = "/v1/embeddings"
	OpenAiRequestUnknownPath         OpenAiRequestPath = ""
)

func NewOpenAiRequestTypeFromPath(path string) OpenAiRequestType {
	requestType := OpenAiRequestUnknownType
	switch path {
	case string(OpenAiRequestCompletionsPath):
		requestType = OpenAiRequestCompletionsType
	case string(OpenAiRequestChatCompletionsPath):
		requestType = OpenAiRequestChatCompletionsType
	case string(OpenAiRequestEmbeddingsPath):
		requestType = OpenAiRequestEmbeddingsType
	}
	return requestType
}
