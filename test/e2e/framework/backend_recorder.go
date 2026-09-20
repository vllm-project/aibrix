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
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	mockBackendPort         = "8000"
	mockBackendRecorderPath = "debug/requests"
)

// BackendRequestRecord is the request data captured by a mock backend. The
// recorder is deliberately queried by request ID so parallel e2e tests never
// assert against another test's traffic.
type BackendRequestRecord struct {
	Sequence      int64             `json:"sequence"`
	RequestID     string            `json:"request_id"`
	Path          string            `json:"path"`
	Headers       map[string]string `json:"headers"`
	RawBodyBase64 string            `json:"raw_body_base64"`
	ParsedJSON    json.RawMessage   `json:"parsed_json"`
	Pod           string            `json:"pod"`
	Engine        string            `json:"engine"`
	Role          string            `json:"role"`
	Outcome       string            `json:"outcome"`
	StatusCode    int               `json:"status_code"`
	Error         string            `json:"error"`
	DelayMS       int               `json:"delay_ms"`
}

// BackendRequestRecords returns one mock backend pod's request records for a
// single request ID through the Kubernetes Pod proxy.
func BackendRequestRecords(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, podName, requestID string,
) ([]BackendRequestRecord, error) {
	raw, err := client.CoreV1().Pods(namespace).ProxyGet(
		"http", podName, mockBackendPort, mockBackendRecorderPath,
		map[string]string{"request_id": requestID},
	).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("read backend recorder from pod %s: %w", podName, err)
	}

	var records []BackendRequestRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("decode backend recorder response from pod %s: %w", podName, err)
	}
	return records, nil
}

// WaitForBackendRequestRecords waits for exactly the requested number of
// recorder entries. A backend can finish after the gateway has timed out, so
// polling avoids a timing race while retaining request-level isolation.
func WaitForBackendRequestRecords(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, podName, requestID string,
	expected int,
	timeout time.Duration,
) ([]BackendRequestRecord, error) {
	var records []BackendRequestRecord
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true,
		func(ctx context.Context) (bool, error) {
			records, lastErr = BackendRequestRecords(ctx, client, namespace, podName, requestID)
			if lastErr != nil {
				return false, nil
			}
			if len(records) != expected {
				return false, nil
			}
			for _, record := range records {
				if record.Outcome == "" {
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("wait for %d backend records for request %s on pod %s: %w", expected, requestID, podName, err)
	}
	return records, nil
}
