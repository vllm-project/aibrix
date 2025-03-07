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
	HeaderErrorStreaming                 = "x-error-streaming"
	HeaderErrorNoStreamOptions           = "x-error-no-stream-options"
	HeaderErrorStreamOptionsIncludeUsage = "x-error-no-stream-options-include-usage"

	// Request & Target Headers
	HeaderWentIntoReqHeaders = "x-went-into-req-headers"
	HeaderTargetPod          = "target-pod"
	HeaderRoutingStrategy    = "routing-strategy"

	// RPM & TPM Update Errors
	HeaderUpdateTPM        = "x-update-tpm"
	HeaderUpdateRPM        = "x-update-rpm"
	HeaderErrorRPMExceeded = "x-error-rpm-exceeded"
	HeaderErrorTPMExceeded = "x-error-tpm-exceeded"
	HeaderErrorIncrRPM     = "x-error-incr-rpm"
	HeaderErrorIncrTPM     = "x-error-incr-tpm"

	// Rate Limiting defaults
	DefaultRPM           = 100
	DefaultTPMMultiplier = 1000

	// Envs
	EnvRoutingAlgorithm = "ROUTING_ALGORITHM"
)

var (
	ErrorUnknownResponse = errors.New("unknown response")
	requestBuffers       sync.Map // Thread-safe map to track buffers per request
)
