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

package prefill

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type prefillAddressTransport func(*http.Request) (*http.Response, error)

func (transport prefillAddressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestDefaultExecutorPrefillAddress(t *testing.T) {
	for _, test := range []struct {
		name string
		host string
		url  string
	}{
		{"IPv4", "127.0.0.1", "http://127.0.0.1:8000/v1/chat/completions"},
		{"DNS", "worker.example", "http://worker.example:8000/v1/chat/completions"},
		{"IPv6", "2001:db8::1", "http://[2001:db8::1]:8000/v1/chat/completions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requestURL string
			client := &http.Client{Transport: prefillAddressTransport(func(request *http.Request) (*http.Response, error) {
				requestURL = request.URL.String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("{}")),
					Header:     make(http.Header),
				}, nil
			})}
			tracker := pd.NewPrefillRequestTracker()
			tracker.AddPrefillRequest("request", "prefill-worker")
			executor := NewDefaultExecutor(client, tracker, 5)
			routingContext := &types.RoutingContext{
				Context:   context.Background(),
				RequestID: "request",
				Model:     "model",
				ReqPath:   "/v1/chat/completions",
				ReqBody:   []byte(`{"model":"model","messages":[]}`),
			}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "prefill-worker",
					Labels: map[string]string{constants.ModelLabelPort: "8000"},
				},
				Status: v1.PodStatus{PodIP: test.host},
			}
			require.NoError(t, executor.Execute(routingContext, pod, "default", LogContext{}))
			require.Equal(t, test.url, requestURL)
		})
	}
}
