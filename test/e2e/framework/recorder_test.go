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
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
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
