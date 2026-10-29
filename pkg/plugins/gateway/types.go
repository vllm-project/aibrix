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
	// HeaderErrorPDPrefill marks a response the gateway generated because the
	// prefill leg of a PD request failed, so the failure is distinguishable
	// from a 5xx the decode pod produced itself.
	HeaderErrorPDPrefill = "x-error-pd-prefill"
	// HeaderErrorPDDecode marks a response the gateway generated because the
	// decode leg of a PD request stopped responding after its prefill leg
	// succeeded (the decode watchdog).
	HeaderErrorPDDecode = "x-error-pd-decode"

	// Model & Deployment Headers
	HeaderErrorNoModelInRequest = "x-error-no-model-in-request"
	HeaderErrorNoModelBackends  = "x-error-no-model-backends"

	// Streaming Headers
	HeaderErrorStream                    = "x-error-stream"
	HeaderErrorStreaming                 = "x-error-streaming"
	HeaderErrorStreamOptionsIncludeUsage = "x-error-no-stream-options-include-usage"

	// Multipart/Audio Headers
	HeaderErrorMultipartParsing = "x-error-multipart-parsing"

	// Video Job Headers
	HeaderErrorVideoNotFound          = "x-error-video-not-found"
	HeaderErrorVideoJobPodUnavailable = "x-error-video-job-pod-unavailable"

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
	// HeaderPriorityTier declares the priority tier of the request (for
	// example batch or background) so the gateway can map it to the upstream
	// vLLM request priority. See gateway_req_priority.go.
	HeaderPriorityTier = "x-aibrix-priority-tier"
	// HeaderSessionID aliases the shared session-affinity header used by request parsing and routing.
	HeaderSessionID = constants.HeaderSessionID
	// HeaderSessionKey aliases the shared opaque session-key header.
	HeaderSessionKey  = constants.HeaderSessionKey
	HeaderTraceParent = "traceparent"
	// HeaderMockPDFailure is a test-only header forwarded to mock PD backends.
	HeaderMockPDFailure = "x-aibrix-mock-fail"

	// RPM & TPM Update Errors
	HeaderUpdateTPM        = "x-update-tpm"
	HeaderUpdateRPM        = "x-update-rpm"
	HeaderErrorRPMExceeded = "x-error-rpm-exceeded"
	HeaderErrorTPMExceeded = "x-error-tpm-exceeded"
	HeaderErrorIncrRPM     = "x-error-incr-rpm"
	HeaderErrorIncrTPM     = "x-error-incr-tpm"
	HeaderErrorGetRPM      = "x-error-get-rpm"
	HeaderErrorGetTPM      = "x-error-get-tpm"

	// Model RPS Errors
	HeaderErrorModelRPSExceeded = "x-error-model-rps-exceeded"
	HeaderErrorIncrModelRPS     = "x-error-incr-model-rps"

	// Per-replica inflight Errors
	HeaderErrorReplicaInflightExceeded = "x-error-model-replica-inflight-exceeded"

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
	ErrorCodeInvalidAPIKey              = "invalid_api_key"
	ErrorCodeModelNotFound              = "model_not_found"
	ErrorCodeRateLimitExceeded          = "rate_limit_exceeded"
	ErrorCodeServiceUnavailable         = "service_unavailable"
	ErrorCodeExternalPolicyDenied       = "external_policy_denied"
	ErrorCodeExternalRouterUnavailable  = "external_router_unavailable"
	ErrorCodeVideoNotFound              = "video_not_found"
	ErrorCodeVideoJobPodUnavailable     = "video_job_pod_unavailable"
	ErrorCodeReplicaInflightExceeded    = "replica_inflight_exceeded"
	ErrorCodeUnsupportedRoutingStrategy = "unsupported_routing_strategy"

	// Embedding Constraints
	// https://github.com/openai/openai-go/blob/main/embedding.go#L126
	//
	// The token-count limits from that reference (8192 per input, 300000 per
	// batch) are not enforced here: they are denominated in the target model's
	// tokens, which the gateway cannot measure. See validateStringInputs.
	MaxArrayDimensions = 2048

	// Request Paths, grouped by modality. Add a new endpoint to its group.

	// Text generation
	PathChatCompletions      = "/v1/chat/completions"
	PathResponses            = "/v1/responses"
	PathMessages             = "/v1/messages"
	PathMessagesCountTokens  = "/v1/messages/count_tokens"
	PathCompletions          = "/v1/completions"
	PathChatCompletionsBatch = "/v1/chat/completions/batch"

	// Embeddings, ranking, classification and scoring
	PathEmbeddings = "/v1/embeddings"
	PathRerank     = "/v1/rerank"
	PathClassify   = "/v1/classify"
	// PathNativeClassify is where vLLM serves classification; PathClassify is SGLang's
	// OpenAI-style route.
	PathNativeClassify = "/classify"
	// PathDecisions is the typed-question scoring endpoint (choice, score, yes_no) served by
	// vLLM and SGLang. It does not stream and generates no tokens.
	PathDecisions = "/v1/decisions"
	// PathSystemOne is the System One compatible form of PathDecisions, served by vLLM and
	// SGLang: a "state" and a map of noul, choice and score questions keyed by caller ids. It
	// does not stream, and neither engine's usage reports a total, only
	// input_tokens/output_tokens.
	PathSystemOne = "/v1/systemone"
	// PathScore is SGLang's scoring endpoint: label-token probabilities or classifier
	// logits for each query/item pair, pointwise or setwise. It does not stream.
	PathScore = "/v1/score"

	// Engine-native utilities
	PathTokenize = "/tokenize"
	PathPooling  = "/pooling"

	// Image
	PathImagesGenerations = "/v1/images/generations"
	// PathImagesEdits is the OpenAI-compatible image edit endpoint. Unlike PathImagesGenerations
	// it takes multipart/form-data, because the source images are uploaded as files.
	PathImagesEdits = "/v1/images/edits"

	// Audio
	PathAudioTranscriptions = "/v1/audio/transcriptions"
	PathAudioTranslations   = "/v1/audio/translations"
	// PathAudioSpeech is the OpenAI-compatible speech generation endpoint. Unlike the
	// transcription and translation endpoints it takes a JSON request and returns binary audio,
	// often streamed in several chunks, so its response is never parsed as JSON or SSE.
	PathAudioSpeech = "/v1/audio/speech"
	// PathAudioSpeechBatch is the batch form of PathAudioSpeech: one JSON request holds
	// many items and the response is one JSON document with a base64-encoded result per item.
	// isLanguageRequest matches it through the PathAudioSpeech prefix, so it is never parsed.
	PathAudioSpeechBatch = "/v1/audio/speech/batch"
	// PathAudioVoices is the engine's catalog of custom voices: GET lists them, POST uploads one
	// as multipart/form-data and DELETE PathAudioVoices/{name} removes one. None of these requests
	// names a model, and two of them have no body to carry one, so the gateway reads it from the
	// model query parameter (see audioVoicesModel). The bodies are forwarded unparsed.
	PathAudioVoices = "/v1/audio/voices"

	// Video
	PathVideoGenerations = "/v1/video/generations"
	// PathVideos and PathVideosSync are vLLM-Omni's native Videos API (multipart/form-data),
	// distinct from the OpenAI/Sora-shaped PathVideoGenerations (JSON) above. PathVideos also
	// covers its GET/DELETE sub-resources (/v1/videos/{id}, /v1/videos/{id}/content) via
	// prefix matching in isLanguageRequest and extractVideoIDFromPath.
	PathVideos     = "/v1/videos"
	PathVideosSync = "/v1/videos/sync"

	// Engine-specific paths (xdit), image and video generation
	PathXditGenerate      = "/generate"
	PathXditGenerateVideo = "/generatevideo"

	// Engine Types
	EngineXdit   = "xdit"
	EngineVLLM   = "vllm"
	EngineSGLang = "sglang"
	EngineTRTLLM = "trtllm"
)

var (
	ErrorUnknownResponse = errors.New("unknown response")
	requestBuffers       sync.Map // Thread-safe map to track buffers per request
	streamBuffers        sync.Map // Thread-safe map to track the trailing partial SSE line per streaming request
	// streamUsage holds the latest usage of a streaming response that has not ended
	// yet. With stream_options.continuous_usage_stats (vLLM, SGLang) every chunk carries
	// a running usage object, so usage alone does not mean the stream is over.
	streamUsage sync.Map
)
