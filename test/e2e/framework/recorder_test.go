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

package e2eframework

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const recorderFixture = `[
  {
    "sequence": 2,
    "request_id": "request-1",
    "path": "/v1/chat/completions",
    "raw_body_base64": "eyJtb2RlbCI6Im1vZGVsIn0=",
    "parsed_json": {"model": "model", "prompt": "hello"},
    "pod": "decode-pod",
    "engine": "vllm",
    "role": "decode",
    "outcome": "success",
    "status_code": 200,
    "error": "",
    "response": {"id": "decode-response"}
  },
  {
    "sequence": 1,
    "request_id": "request-1",
    "path": "/v1/chat/completions",
    "raw_body_base64": "eyJtb2RlbCI6Im1vZGVsIn0=",
    "parsed_json": {"model": "model", "prompt": "hello"},
    "pod": "prefill-pod",
    "engine": "vllm",
    "role": "prefill",
    "outcome": "success",
    "status_code": 200,
    "error": "",
    "response": {"id": "prefill-response"}
  }
]`

func TestDecodeMockRequestRecords(t *testing.T) {
	records, err := DecodeMockRequestRecords([]byte(recorderFixture))

	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, 1, records[0].Sequence)
	require.Equal(t, "prefill", records[0].Role)
	require.Equal(t, 2, records[1].Sequence)
	require.Equal(t, "decode", records[1].Role)
	require.JSONEq(t, `{"model":"model","prompt":"hello"}`, string(records[0].ParsedJSON))
	require.JSONEq(t, `{"id":"decode-response"}`, string(records[1].Response))
}

func TestClassifyPDRecordsTreatsEmptyOutcomeAsPending(t *testing.T) {
	records, err := DecodeMockRequestRecords([]byte(`[
		{"sequence": 1, "request_id": "request-1", "pod": "prefill-pod", "engine": "vllm", "role": "prefill"},
		{"sequence": 2, "request_id": "request-1", "pod": "decode-pod", "engine": "vllm", "role": "decode", "outcome": "success", "status_code": 200}
	]`))
	require.NoError(t, err)

	ready, err := classifyPDRecords(records, "request-1", "vllm-aibrix-shfs", "vllm", "prefill")

	require.False(t, ready)
	require.NoError(t, err)
}

func TestClassifyPDRecordsReportsRejectedRecordDetails(t *testing.T) {
	records := []MockRequestRecord{{
		RequestID:  "request-1",
		Pod:        "prefill-pod",
		Engine:     "vllm",
		Role:       "prefill",
		Outcome:    "rejected",
		StatusCode: http.StatusBadRequest,
		Error:      "invalid SHFS handoff",
	}}

	ready, err := classifyPDRecords(records, "request-1", "vllm-aibrix-shfs", "vllm", "prefill")

	require.False(t, ready)
	require.Error(t, err)
	assert.ErrorContains(t, err, "prefill-pod")
	assert.ErrorContains(t, err, "prefill")
	assert.ErrorContains(t, err, "400")
	assert.ErrorContains(t, err, "invalid SHFS handoff")
}

func TestQueryMockRequestsUsesPodProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/namespaces/test/pods/mock-pod/proxy/debug/requests", r.URL.Path)
		require.Equal(t, "request-1", r.URL.Query().Get("request_id"))
		_, _ = fmt.Fprint(w, recorderFixture)
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, APIPath: "/api", ContentConfig: rest.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}, NegotiatedSerializer: scheme.Codecs}})
	require.NoError(t, err)

	records, err := QueryMockRequests(context.Background(), client, "test", "mock-pod", "request-1")

	require.NoError(t, err)
	require.Len(t, records, 2)
}

func TestWaitForSuccessfulPDLegsReturnsOneLegPerPod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/prefill-pod/") {
			_, _ = fmt.Fprint(w, `[{"sequence":1,"request_id":"request-1","pod":"prefill-pod","engine":"vllm","role":"prefill","outcome":"success","status_code":200}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"sequence":2,"request_id":"request-1","pod":"decode-pod","engine":"vllm","role":"decode","outcome":"success","status_code":200}]`)
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, APIPath: "/api", ContentConfig: rest.ContentConfig{GroupVersion: &schema.GroupVersion{Group: "", Version: "v1"}, NegotiatedSerializer: scheme.Codecs}})
	require.NoError(t, err)

	prefill, decode := WaitForSuccessfulPDLegs(t, client, "test", "prefill-pod", "decode-pod", "request-1", "vllm-aibrix-shfs", "vllm")

	require.Equal(t, 1, prefill.Sequence)
	require.Equal(t, 2, decode.Sequence)
}

func TestDecodeMockRequestRecordsRejectsMalformedJSON(t *testing.T) {
	_, err := DecodeMockRequestRecords([]byte(`[{"sequence":`))

	require.Error(t, err)
	require.ErrorContains(t, err, "decode mock request records")
}

func TestSelectSuccessfulPDLegsWithoutContractField(t *testing.T) {
	records, err := DecodeMockRequestRecords([]byte(recorderFixture))
	require.NoError(t, err)

	prefill, decode, err := SelectSuccessfulPDLegs(records, "request-1", "vllm-aibrix-shfs", "vllm")

	require.NoError(t, err)
	require.Equal(t, "prefill-pod", prefill.Pod)
	require.Equal(t, "decode-pod", decode.Pod)
}

func TestSelectSuccessfulPDLegsRequiresExactlyOneSuccessfulLegPerRole(t *testing.T) {
	base, err := DecodeMockRequestRecords([]byte(recorderFixture))
	require.NoError(t, err)

	tests := []struct {
		name   string
		modify func([]MockRequestRecord) []MockRequestRecord
	}{
		{
			name: "missing prefill",
			modify: func(records []MockRequestRecord) []MockRequestRecord {
				return records[1:]
			},
		},
		{
			name: "missing decode",
			modify: func(records []MockRequestRecord) []MockRequestRecord {
				return records[:1]
			},
		},
		{
			name: "duplicate prefill",
			modify: func(records []MockRequestRecord) []MockRequestRecord {
				return append(records, records[0])
			},
		},
		{
			name: "duplicate decode",
			modify: func(records []MockRequestRecord) []MockRequestRecord {
				return append(records, records[1])
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := SelectSuccessfulPDLegs(test.modify(append([]MockRequestRecord(nil), base...)), "request-1", "vllm-aibrix-shfs", "vllm")

			require.Error(t, err)
		})
	}
}

func TestSelectSuccessfulPDLegsFiltersNonSuccessfulRecords(t *testing.T) {
	records, err := DecodeMockRequestRecords([]byte(recorderFixture))
	require.NoError(t, err)

	records[0].Outcome = "failed"
	records[0].StatusCode = http.StatusInternalServerError
	_, _, err = SelectSuccessfulPDLegs(records, "request-1", "vllm-aibrix-shfs", "vllm")

	require.Error(t, err)
}
