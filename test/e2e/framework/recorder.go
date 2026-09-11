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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// MockRequestRecord is a JSON-compatible request record returned by the mock app.
type MockRequestRecord struct {
	Sequence      int             `json:"sequence"`
	RequestID     string          `json:"request_id"`
	Path          string          `json:"path"`
	RawBodyBase64 string          `json:"raw_body_base64"`
	ParsedJSON    json.RawMessage `json:"parsed_json"`
	Pod           string          `json:"pod"`
	Engine        string          `json:"engine"`
	Role          string          `json:"role"`
	Outcome       string          `json:"outcome"`
	StatusCode    int             `json:"status_code"`
	Error         string          `json:"error"`
	Response      json.RawMessage `json:"response"`
}

// DecodeMockRequestRecords decodes and orders records returned by the mock app.
func DecodeMockRequestRecords(body []byte) ([]MockRequestRecord, error) {
	var records []MockRequestRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decode mock request records: %w", err)
	}

	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Sequence < records[j].Sequence
	})
	return records, nil
}

// SelectSuccessfulPDLegs returns the unique successful prefill and decode legs.
func SelectSuccessfulPDLegs(
	records []MockRequestRecord,
	requestID, engine string,
) (prefill, decode MockRequestRecord, err error) {
	var prefills, decodes []MockRequestRecord
	for _, record := range records {
		if record.RequestID != requestID ||
			record.Engine != engine ||
			record.Outcome != "success" ||
			record.StatusCode != http.StatusOK {
			continue
		}

		switch record.Role {
		case "prefill":
			prefills = append(prefills, record)
		case "decode":
			decodes = append(decodes, record)
		}
	}

	if len(prefills) != 1 || len(decodes) != 1 {
		return MockRequestRecord{}, MockRequestRecord{}, fmt.Errorf(
			"expected exactly one successful prefill and decode leg, got %d prefill and %d decode",
			len(prefills), len(decodes),
		)
	}
	return prefills[0], decodes[0], nil
}

// QueryMockRequests reads request records through the Kubernetes pod proxy.
func QueryMockRequests(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, podName, requestID string,
) ([]MockRequestRecord, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client is nil")
	}
	body, err := client.CoreV1().RESTClient().Get().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("proxy").
		Suffix("debug/requests").
		Param("request_id", requestID).
		Do(ctx).
		Raw()
	if err != nil {
		return nil, fmt.Errorf(
			"query mock requests for pod %q request ID %q: %w",
			podName,
			requestID,
			err,
		)
	}
	records, err := DecodeMockRequestRecords(body)
	if err != nil {
		return nil, fmt.Errorf(
			"query mock requests for pod %q request ID %q: %w",
			podName,
			requestID,
			err,
		)
	}
	return records, nil
}

func classifyPDRecords(records []MockRequestRecord, requestID, engine, role string) (bool, error) {
	matching := false
	successful := 0
	for _, record := range records {
		if record.RequestID != requestID || record.Engine != engine || record.Role != role {
			continue
		}
		matching = true
		if record.Outcome == "" {
			continue
		}
		if record.Outcome != "success" || record.StatusCode != http.StatusOK {
			return false, fmt.Errorf(
				"mock recorder rejected/failed pod %q role %q status code %d: %s",
				record.Pod,
				record.Role,
				record.StatusCode,
				record.Error,
			)
		}
		successful++
	}

	if !matching || successful != 1 {
		return false, nil
	}
	return true, nil
}

// WaitForSuccessfulPDLegs waits for one successful HTTP-200 recorder entry per PD role.
func WaitForSuccessfulPDLegs(
	t *testing.T,
	client kubernetes.Interface,
	namespace, prefillPod, decodePod, requestID, engine string,
) (prefill, decode MockRequestRecord) {
	t.Helper()
	var lastPrefill, lastDecode []byte
	var successfulPrefill, successfulDecode []MockRequestRecord
	err := wait.PollUntilContextTimeout(
		context.Background(),
		time.Second,
		2*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			prefillRecords, err := QueryMockRequests(ctx, client, namespace, prefillPod, requestID)
			if err != nil {
				if apierrors.IsNotFound(err) {
					prefillRecords = nil
				} else {
					t.Logf("recorder query for prefill pod %q failed; retrying: %v", prefillPod, err)
					prefillRecords = nil
				}
			}
			decodeRecords, err := QueryMockRequests(ctx, client, namespace, decodePod, requestID)
			if err != nil {
				if apierrors.IsNotFound(err) {
					decodeRecords = nil
				} else {
					t.Logf("recorder query for decode pod %q failed; retrying: %v", decodePod, err)
					decodeRecords = nil
				}
			}
			lastPrefill, _ = json.Marshal(prefillRecords)
			lastDecode, _ = json.Marshal(decodeRecords)

			if ready, err := classifyPDRecords(prefillRecords, requestID, engine, "prefill"); err != nil {
				return false, err
			} else if !ready {
				return false, nil
			}
			if ready, err := classifyPDRecords(decodeRecords, requestID, engine, "decode"); err != nil {
				return false, err
			} else if !ready {
				return false, nil
			}
			successfulPrefill = prefillRecords
			successfulDecode = decodeRecords
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf(
			"wait for successful PD legs for request ID %q: %v; "+
				"last prefill recorder JSON: %s; last decode recorder JSON: %s",
			requestID,
			err,
			lastPrefill,
			lastDecode,
		)
	}
	prefill, decode, err = SelectSuccessfulPDLegs(
		append(successfulPrefill, successfulDecode...),
		requestID,
		engine,
	)
	if err != nil {
		t.Fatalf("select successful PD legs for request ID %q: %v", requestID, err)
	}
	return prefill, decode
}
