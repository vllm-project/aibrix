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

import (
	"errors"
	"sync"

	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	HeaderErrorInvalidRouting = "x-error-invalid-routing-strategy"

	// General Error Headers
	HeaderErrorUser                  = "x-error-user"
	HeaderErrorRouting               = "x-error-routing"
	HeaderErrorRequestBodyProcessing = "x-error-request-body-processing"
	HeaderErrorResponseUnmarshal     = "x-error-response-unmarshal"
	HeaderErrorResponseUnknown       = "x-error-response-unknown"

	// Model & Deployment Headers
	HeaderErrorNoModelInRequest = "x-error-no-model-in-request"
	HeaderErrorNoModelBackends  = "x-error-no-model-backends"

	// Streaming Headers
	HeaderErrorStream                    = "x-error-stream"
	HeaderErrorStreaming                 = "x-error-streaming"
	HeaderErrorStreamOptionsIncludeUsage = "x-error-no-stream-options-include-usage"

	// Multipart/Audio Headers
	HeaderErrorMultipartParsing = "x-error-multipart-parsing"

	// Request & Target Headers
	HeaderWentIntoReqHeaders  = "x-went-into-req-headers"
	HeaderTargetPodIP         = "target-pod-ip"
	HeaderTargetPod           = "target-pod"
	HeaderRoutingStrategy     = "routing-strategy"
	HeaderRequestID           = "request-id"
	HeaderModel               = "model"
	HeaderExternalFilter      = "external-filter"
	HeaderConfigProfile       = "config-profile"
	HeaderAIBrixConfigProfile = "x-aibrix-config-profile"
	// HeaderSessionID aliases the shared session-affinity header used by request parsing and routing.
	HeaderSessionID = constants.HeaderSessionID
	// HeaderSessionKey aliases the shared opaque session-key header.
	HeaderSessionKey  = constants.HeaderSessionKey
	HeaderTraceParent = "traceparent"

	// RPM & TPM Update Errors
	HeaderUpdateTPM        = "x-update-tpm"
	HeaderUpdateRPM        = "x-update-rpm"
	HeaderErrorRPMExceeded = "x-error-rpm-exceeded"
	HeaderErrorTPMExceeded = "x-error-tpm-exceeded"
	HeaderErrorIncrRPM     = "x-error-incr-rpm"
	HeaderErrorIncrTPM     = "x-error-incr-tpm"

	// Model RPS Errors
	HeaderErrorModelRPSExceeded = "x-error-model-rps-exceeded"
	HeaderErrorIncrModelRPS     = "x-error-incr-model-rps"

	// Rate Limiting defaults
	DefaultRPM           = 100
	DefaultTPMMultiplier = 1000

	// Envs
	EnvRoutingAlgorithm = "ROUTING_ALGORITHM"

	// OpenAI Error Types
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeApi            = "api_error"
	ErrorTypeOverloaded     = "overloaded_error"

	// OpenAI Error Codes
	ErrorCodeInvalidAPIKey      = "invalid_api_key"
	ErrorCodeModelNotFound      = "model_not_found"
	ErrorCodeRateLimitExceeded  = "rate_limit_exceeded"
	ErrorCodeServiceUnavailable = "service_unavailable"

	// Embedding Constraints
	// https://github.com/openai/openai-go/blob/main/embedding.go#L126
	//
	// The token-count limits from that reference (8192 per input, 300000 per
	// batch) are not enforced here: they are denominated in the target model's
	// tokens, which the gateway cannot measure. See validateStringInputs.
	MaxArrayDimensions = 2048

	// Request Paths
	PathChatCompletions     = "/v1/chat/completions"
	PathResponses           = "/v1/responses"
	PathMessages            = "/v1/messages"
	PathCompletions         = "/v1/completions"
	PathEmbeddings          = "/v1/embeddings"
	PathImagesGenerations   = "/v1/images/generations"
	PathVideoGenerations    = "/v1/video/generations"
	PathAudioTranscriptions = "/v1/audio/transcriptions"
	PathAudioTranslations   = "/v1/audio/translations"
	PathRerank              = "/v1/rerank"
	PathClassify            = "/v1/classify"

	// Engine-specific paths (xdit)
	PathXditGenerate      = "/generate"
	PathXditGenerateVideo = "/generatevideo"

	// Engine Types
	EngineXdit = "xdit"
)

var (
	ErrorUnknownResponse = errors.New("unknown response")
	requestBuffers       sync.Map // Thread-safe map to track buffers per request
	streamBuffers        sync.Map // Thread-safe map to track the trailing partial SSE line per streaming request
)
