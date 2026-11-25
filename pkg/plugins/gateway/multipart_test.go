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
	"mime/multipart"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMultipartForm encodes fields as a multipart/form-data body and returns
// the body along with the Content-Type header (including boundary).
func buildMultipartForm(t *testing.T, fields map[string]string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	return buf.Bytes(), w.FormDataContentType()
}

// buildMultipartFormWithFile is buildMultipartForm plus one file part, as an image edit
// request carries its source image.
func buildMultipartFormWithFile(t *testing.T, fields map[string]string, fileField, fileName string, content []byte) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	part, err := w.CreateFormFile(fileField, fileName)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes(), w.FormDataContentType()
}

// TestParseMultipartFormData_IgnoresStreamForVideoPaths guards against a
// client (or an SDK reusing a generic multipart helper across audio/video
// calls) sending stream=true on the async, never-streamed Videos API. If that
// field were honored, HandleResponseBody would take the SSE branch instead of
// calling handleVideoJobResponseBody, and the job would never be registered --
// breaking follow-up GET/DELETE calls with a spurious "video not found".
func TestParseMultipartFormData_IgnoresStreamForVideoPaths(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStream bool
	}{
		{"videos create ignores stream field", PathVideos, false},
		{"videos sync create ignores stream field", PathVideosSync, false},
		{"videos create with query string still ignores stream field", PathVideos + "?foo=bar", false},
		{"audio transcription honors stream field", PathAudioTranscriptions, true},
		// Multi-stage image edit pipelines answer stream=true with SSE, so the flag has to
		// reach HandleResponseBody.
		{"image edits honors stream field", PathImagesEdits, true},
		{"image edits with query string honors stream field", PathImagesEdits + "?foo=bar", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := buildMultipartForm(t, map[string]string{
				"model":  "test-model",
				"stream": "true",
			})

			model, stream, errRes := parseMultipartFormData("req-1", tt.path, contentType, body)

			assert.Nil(t, errRes)
			assert.Equal(t, "test-model", model)
			assert.Equal(t, tt.wantStream, stream)
		})
	}
}
