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

package encode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
)

// gatedTransport answers encode requests with 200. Requests to a gated host
// wait until release is closed. It records the host each request ID went to.
type gatedTransport struct {
	gated   string
	release chan struct{}
	mu      sync.Mutex
	hosts   map[string]string
	arrived chan string
}

func newGatedTransport(gated string) *gatedTransport {
	return &gatedTransport{gated: gated, release: make(chan struct{}), hosts: map[string]string{}, arrived: make(chan string, 16)}
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := req.Header.Get("X-Request-Id")
	g.mu.Lock()
	g.hosts[id] = req.URL.Host
	g.mu.Unlock()
	g.arrived <- req.URL.Host
	if req.URL.Host == g.gated {
		<-g.release
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

func (g *gatedTransport) host(id string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hosts[id]
}

func encoderPod(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{constants.ModelLabelPort: "8000"}},
		Status:     v1.PodStatus{PodIP: ip},
	}
}

func encodeRequest(t *testing.T, id string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), "test", "qwen-vl", "", id, "user")
	t.Cleanup(ctx.Delete)
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)
	return ctx
}

const imageItem = `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`

// Encoder a is held busy by the first request. With least_request the next
// requests go to the idle encoder b; round-robin sends the third back to a.
func TestEncoderSelectionUnderLoad(t *testing.T) {
	const hostA, hostB = "10.0.0.1:8000", "10.0.0.2:8000"
	for _, tc := range []struct {
		policy    string
		wantThird string
	}{
		{policy: PolicyLeastRequest, wantThird: hostB},
		{policy: PolicyRoundRobin, wantThird: hostA},
		{policy: "", wantThird: hostA},
	} {
		t.Run("policy="+tc.policy, func(t *testing.T) {
			t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", tc.policy)
			transport := newGatedTransport(hostA)
			executor := NewExecutor(&http.Client{Transport: transport})
			encoders := []*v1.Pod{encoderPod("encode-a", "10.0.0.1"), encoderPod("encode-b", "10.0.0.2")}

			first := make(chan error, 1)
			req1 := encodeRequest(t, "r1")
			go func() {
				first <- executor.Execute(req1, encoders, []string{imageItem}, 5*time.Second)
			}()
			require.Equal(t, hostA, <-transport.arrived, "the first item goes to the first encoder")

			require.NoError(t, executor.Execute(encodeRequest(t, "r2"), encoders, []string{imageItem}, 5*time.Second))
			<-transport.arrived
			assert.Equal(t, hostB, transport.host("r2-encode-0"))

			third := make(chan error, 1)
			req3 := encodeRequest(t, "r3")
			go func() {
				third <- executor.Execute(req3, encoders, []string{imageItem}, 5*time.Second)
			}()
			assert.Equal(t, tc.wantThird, <-transport.arrived)

			close(transport.release)
			require.NoError(t, <-first)
			require.NoError(t, <-third)
			executor.mu.Lock()
			defer executor.mu.Unlock()
			assert.Empty(t, executor.inflight, "every item is released")
		})
	}
}

// The items of one request spread over idle encoders.
func TestLeastRequestSpreadsItemsOfOneRequest(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyLeastRequest)
	executor := NewExecutor(nil)
	encoders := []*v1.Pod{encoderPod("encode-a", "10.0.0.1"), encoderPod("encode-b", "10.0.0.2"), encoderPod("encode-c", "10.0.0.3")}

	picks := executor.assign(encoders, 3)
	seen := map[string]bool{}
	for _, pod := range picks {
		seen[pod.Name] = true
	}
	assert.Len(t, seen, 3, "three items on three idle encoders")
	for _, pod := range picks {
		executor.release(pod)
	}
	assert.Empty(t, executor.inflight)
}

// A nil pod among the encoders is skipped under least_request; an item that
// can only go to a nil pod fails in encode, as under round-robin, instead of
// panicking.
func TestLeastRequestSkipsNilEncoders(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyLeastRequest)
	executor := NewExecutor(&http.Client{Transport: newGatedTransport("")})
	b := encoderPod("encode-b", "10.0.0.2")

	require.NoError(t, executor.Execute(encodeRequest(t, "r1"), []*v1.Pod{nil, b}, []string{imageItem, imageItem}, 5*time.Second))
	err := executor.Execute(encodeRequest(t, "r2"), []*v1.Pod{nil}, []string{imageItem}, 5*time.Second)
	require.ErrorContains(t, err, "encode pod is nil")
	assert.Empty(t, executor.inflight)
}

// Under round-robin too, an item sent to a nil pod fails instead of panicking.
func TestRoundRobinNilEncoderFails(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyRoundRobin)
	executor := NewExecutor(&http.Client{Transport: newGatedTransport("")})
	err := executor.Execute(encodeRequest(t, "r1"), []*v1.Pod{nil}, []string{imageItem}, 5*time.Second)
	require.ErrorContains(t, err, "encode pod is nil")
}

// statusTransport answers every encode request with status and counts the
// requests per host.
type statusTransport struct {
	status int
	mu     sync.Mutex
	hosts  map[string]int
}

func (s *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hosts[req.URL.Host]++
	s.mu.Unlock()
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

// With every encoder idle, ties rotate: sequential single-item requests spread
// evenly instead of all going to the first encoder.
func TestLeastRequestRotatesTiesAcrossIdleEncoders(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyLeastRequest)
	transport := &statusTransport{status: http.StatusOK, hosts: map[string]int{}}
	executor := NewExecutor(&http.Client{Transport: transport})
	encoders := []*v1.Pod{encoderPod("encode-a", "10.0.0.1"), encoderPod("encode-b", "10.0.0.2"), encoderPod("encode-c", "10.0.0.3")}

	for i := 0; i < 9; i++ {
		require.NoError(t, executor.Execute(encodeRequest(t, fmt.Sprintf("r%d", i)), encoders, []string{imageItem}, 5*time.Second))
	}
	assert.Equal(t, map[string]int{"10.0.0.1:8000": 3, "10.0.0.2:8000": 3, "10.0.0.3:8000": 3}, transport.hosts)
}

// A failed encode gives its charge back, whether the encoder answered with an
// error or the pod could not be reached at all.
func TestLeastRequestReleasesOnFailure(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyLeastRequest)
	executor := NewExecutor(&http.Client{Transport: &statusTransport{status: http.StatusInternalServerError, hosts: map[string]int{}}})
	encoders := []*v1.Pod{encoderPod("encode-a", "10.0.0.1"), encoderPod("encode-no-ip", "")}

	err := executor.Execute(encodeRequest(t, "r1"), encoders, []string{imageItem, imageItem}, 5*time.Second)
	require.Error(t, err)
	executor.mu.Lock()
	defer executor.mu.Unlock()
	assert.Empty(t, executor.inflight)
}

// Concurrent requests share the counts safely (run with -race) and give every
// charge back.
func TestLeastRequestConcurrentExecute(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", PolicyLeastRequest)
	transport := &statusTransport{status: http.StatusOK, hosts: map[string]int{}}
	executor := NewExecutor(&http.Client{Transport: transport})
	encoders := []*v1.Pod{encoderPod("encode-a", "10.0.0.1"), encoderPod("encode-b", "10.0.0.2"),
		encoderPod("encode-c", "10.0.0.3"), encoderPod("encode-d", "10.0.0.4")}

	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		reqs := make([]*types.RoutingContext, 10)
		for i := range reqs {
			reqs[i] = encodeRequest(t, fmt.Sprintf("g%d-r%d", g, i))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, req := range reqs {
				assert.NoError(t, executor.Execute(req, encoders, []string{imageItem, imageItem, imageItem}, 5*time.Second))
			}
		}()
	}
	wg.Wait()
	executor.mu.Lock()
	defer executor.mu.Unlock()
	assert.Empty(t, executor.inflight)
	total := 0
	for _, n := range transport.hosts {
		total += n
	}
	assert.Equal(t, 20*10*3, total)
}

func TestUnknownEncoderSelectionPolicyUsesRoundRobin(t *testing.T) {
	t.Setenv("AIBRIX_ENCODE_SELECTION_POLICY", "fastest")
	assert.False(t, NewExecutor(nil).leastRequest)
}
