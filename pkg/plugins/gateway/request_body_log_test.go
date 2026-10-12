/*
Copyright 2026 The Aibrix Team.

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
	"flag"
	"io"
	"mime/multipart"
	"strings"
	"testing"
	"unicode/utf8"

	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// captureKlog raises klog's -v to level and collects its output for one test,
// restoring both in t.Cleanup. Verbosity and output are process-wide, so a test
// using this must not run in parallel. The returned func flushes and returns what
// was logged so far.
func captureKlog(t *testing.T, level string) func() string {
	t.Helper()

	fs := flag.NewFlagSet("klog-capture", flag.ContinueOnError)
	klog.InitFlags(fs)
	vFlag := fs.Lookup("v")
	require.NotNil(t, vFlag)
	previous := vFlag.Value.String()
	require.NoError(t, vFlag.Value.Set(level))

	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
		require.NoError(t, vFlag.Value.Set(previous))
	})

	return func() string {
		klog.Flush()
		return logs.String()
	}
}

// requestLogSecret sits next to the syntax error, inside the excerpt sonic quotes.
const requestLogSecret = "SECRET-PROMPT"

const unparsableRequestBody = `{"model":"m","prompt":"` + requestLogSecret + `" "oops":1}`

// Every JSON validator keeps the body out of the normal log levels, including out
// of the parse error text, which quotes the bytes around the failure.
func TestValidateRequestBody_ParseFailureLogsMetadataOnly(t *testing.T) {
	for _, path := range []string{
		PathChatCompletions, PathMessages, PathResponses, PathCompletions, PathEmbeddings,
		PathImagesGenerations, PathVideoGenerations, PathRerank, PathClassify,
		PathDecisions, PathSystemOne, PathTokenize, PathPooling, PathMessagesCountTokens,
	} {
		t.Run(path, func(t *testing.T) {
			logs := captureKlog(t, "0")

			_, _, _, _, errRes := validateRequestBody("req-log", path, []byte(unparsableRequestBody), utils.User{})

			require.NotNil(t, errRes)
			assert.Equal(t, envoyTypePb.StatusCode_BadRequest, errRes.GetImmediateResponse().GetStatus().GetCode())
			assert.Contains(t, errRes.GetImmediateResponse().GetBody(), "error processing request body")

			out := logs()
			assert.Contains(t, out, `requestID="req-log"`)
			assert.Contains(t, out, `endpoint="`+path+`"`)
			assert.Contains(t, out, "requestBodyBytes=")
			assert.NotContains(t, out, requestLogSecret)
		})
	}
}

func TestValidateRequestBody_ParseFailureBodyOnlyAtV6(t *testing.T) {
	logs := captureKlog(t, "6")

	_, _, _, _, errRes := validateRequestBody("req-log", PathChatCompletions, []byte(unparsableRequestBody), utils.User{})

	require.NotNil(t, errRes)
	out := logs()
	assert.Contains(t, out, requestLogSecret)
	assert.Contains(t, out, "requestBodyTruncated=false")
}

func TestValidateRequestBody_ParseFailureBodyIsCappedAtV6(t *testing.T) {
	logs := captureKlog(t, "6")
	const tail = "TAIL-PAST-THE-CAP"
	body := `{"model":"m","prompt":"` + strings.Repeat("a", maxLoggedRequestBytes) + `" "oops":"` + tail + `"}`

	validateRequestBody("req-log", PathChatCompletions, []byte(body), utils.User{})

	out := logs()
	assert.Contains(t, out, "requestBodyTruncated=true")
	assert.NotContains(t, out, tail)
}

func TestTruncateForLog_CutsOnARuneBoundary(t *testing.T) {
	b := append(bytes.Repeat([]byte("a"), maxLoggedRequestBytes-1), "é"...)

	text, truncated := truncateForLog(b)

	assert.True(t, truncated)
	assert.Len(t, text, maxLoggedRequestBytes-1)
	assert.True(t, utf8.ValidString(text))
}

// The parsed prompt is customer text too: its size is logged at V(4), the text
// only at V(6).
func TestValidateRequestBody_PromptOnlyAtV6(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + requestLogSecret + `"}]}`)

	t.Run("v4", func(t *testing.T) {
		logs := captureKlog(t, "4")
		_, _, _, _, errRes := validateRequestBody("req-log", PathChatCompletions, body, utils.User{})
		require.Nil(t, errRes)

		out := logs()
		assert.Contains(t, out, "messageBytes=")
		assert.NotContains(t, out, requestLogSecret)
	})

	t.Run("v6", func(t *testing.T) {
		logs := captureKlog(t, "6")
		_, _, _, _, errRes := validateRequestBody("req-log", PathChatCompletions, body, utils.User{})
		require.Nil(t, errRes)

		out := logs()
		assert.Contains(t, out, requestLogSecret)
		assert.Contains(t, out, "messageTruncated=false")
	})

	t.Run("v6 capped", func(t *testing.T) {
		logs := captureKlog(t, "6")
		const tail = "TAIL-PAST-THE-CAP"
		long := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", maxLoggedRequestBytes) + tail + `"}]}`)
		_, _, _, _, errRes := validateRequestBody("req-log", PathChatCompletions, long, utils.User{})
		require.Nil(t, errRes)

		out := logs()
		assert.Contains(t, out, "messageTruncated=true")
		assert.NotContains(t, out, tail)
	})
}

// buildMultipartUpload encodes an audio-style upload: a model field, a form value
// and a file part, each carrying its own marker.
func buildMultipartUpload(t *testing.T) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	require.NoError(t, w.WriteField("model", "m"))
	require.NoError(t, w.WriteField("language", "SECRET-FORM-VALUE"))
	part, err := w.CreateFormFile("file", "SECRET-FILENAME.wav")
	require.NoError(t, err)
	_, err = part.Write([]byte("SECRET-FILE-CONTENT"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes(), w.FormDataContentType()
}

func TestMultipartContentsAreNeverLogged(t *testing.T) {
	body, contentType := buildMultipartUpload(t)
	secrets := []string{"SECRET-FORM-VALUE", "SECRET-FILENAME", "SECRET-FILE-CONTENT"}

	t.Run("multipart parser", func(t *testing.T) {
		logs := captureKlog(t, "6")
		model, _, errRes := parseMultipartFormData("req-log", PathAudioTranscriptions, contentType, body)
		require.Nil(t, errRes)
		assert.Equal(t, "m", model)

		out := logs()
		for _, secret := range secrets {
			assert.NotContains(t, out, secret)
		}
	})

	// A multipart upload sent to a JSON endpoint fails JSON parsing, which must not
	// turn the verbose parse-failure diagnostics into a dump of the upload.
	t.Run("sent to a JSON endpoint", func(t *testing.T) {
		logs := captureKlog(t, "6")
		_, _, _, _, errRes := validateRequestBody("req-log", PathChatCompletions, body, utils.User{})
		require.NotNil(t, errRes)

		out := logs()
		assert.Contains(t, out, "requestBodyBytes=")
		for _, secret := range secrets {
			assert.NotContains(t, out, secret)
		}
	})
}
