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
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"k8s.io/klog/v2"
)

// isMultipartFormPath returns true if requestPath is an endpoint whose request
// body is multipart/form-data rather than JSON -- audio transcription and translation,
// image edits, and vLLM-Omni's Videos API create endpoints (PathVideos, PathVideosSync).
func isMultipartFormPath(requestPath string) bool {
	switch pathWithoutQuery(requestPath) {
	case PathAudioTranscriptions, PathAudioTranslations, PathImagesEdits, PathVideos, PathVideosSync:
		return true
	default:
		return false
	}
}

// isMultipartBody reports whether body opens with a multipart boundary delimiter.
func isMultipartBody(body []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(body, "\r\n"), []byte("--"))
}

// isMultipartRequest returns true if the content type indicates multipart form data
func isMultipartRequest(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return strings.HasPrefix(mediaType, "multipart/")
}

// parseMultipartFormData parses multipart/form-data request body and extracts the model field.
// It returns the model name, stream flag, and any processing error response.
//
// requestPath is used to skip the "stream" field for vLLM-Omni's Videos API
// (PathVideos, PathVideosSync): video creation is an async job -- HandleResponseBody
// relies on stream being false there to reach handleVideoJobResponseBody, so a stray
// stream=true field (e.g. from an SDK reusing a generic multipart helper across
// audio/video calls) must not be allowed to route the response down the SSE branch
// instead.
// nolint:nakedret
func parseMultipartFormData(requestID, requestPath, contentType string, requestBody []byte) (model string, stream bool, errRes *extProcPb.ProcessingResponse) {
	const trueStr = "true"
	isVideoPath := pathWithoutQuery(requestPath) == PathVideos || pathWithoutQuery(requestPath) == PathVideosSync

	// Extract boundary from Content-Type
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		klog.ErrorS(err, "failed to parse content-type", "requestID", requestID, "contentType", contentType)
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "invalid content-type header", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "expected multipart/form-data content-type", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	boundary := params["boundary"]
	if boundary == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "missing boundary in content-type", "", "", HeaderErrorMultipartParsing, trueStr)
		return
	}

	// Parse multipart form
	reader := multipart.NewReader(bytes.NewReader(requestBody), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			klog.ErrorS(err, "failed to read multipart part", "requestID", requestID)
			errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "failed to parse multipart form", "", "", HeaderErrorMultipartParsing, trueStr)
			return
		}

		fieldName := part.FormName()

		switch fieldName {
		case "model":
			modelBytes, err := io.ReadAll(part)
			if err != nil {
				klog.ErrorS(err, "failed to read model field", "requestID", requestID)
				errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "failed to read model field", "", "model", HeaderErrorMultipartParsing, trueStr)
				return
			}
			model = strings.TrimSpace(string(modelBytes))

		case "stream":
			if isVideoPath {
				// Video creation never streams; ignore a client-supplied stream field
				// rather than letting it flip HandleResponseBody onto the SSE branch.
				break
			}
			streamBytes, err := io.ReadAll(part)
			if err == nil {
				streamVal := strings.TrimSpace(strings.ToLower(string(streamBytes)))
				stream = streamVal == trueStr || streamVal == "1"
			}
		}

		_ = part.Close()
	}

	// Validate required model field
	if model == "" {
		errRes = buildErrorResponse(envoyTypePb.StatusCode_BadRequest, "'model' is a required property", "", "model", HeaderErrorMultipartParsing, trueStr)
		return
	}

	klog.V(4).InfoS("parseMultipartFormData", "requestID", requestID, "model", model, "stream", stream)
	return
}
