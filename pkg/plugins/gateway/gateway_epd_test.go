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
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// firstPodRouter routes to the first pod it is given and remembers the pods.
type firstPodRouter struct {
	mu   sync.Mutex
	pods []*v1.Pod
}

func (r *firstPodRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	r.mu.Lock()
	r.pods = pods.All()
	r.mu.Unlock()
	ctx.SetTargetPod(pods.All()[0])
	return ctx.TargetAddress(), nil
}

func (r *firstPodRouter) candidates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, pod := range r.pods {
		names = append(names, pod.Name)
	}
	return names
}

type encodeRecorder struct {
	mu    sync.Mutex
	calls int
}

func (e *encodeRecorder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		e.mu.Lock()
		e.calls++
		e.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *encodeRecorder) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func epdServingPod(t *testing.T, name, roleset, role string, srv *httptest.Server) *v1.Pod {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{
			"roleset-name":             roleset,
			"role-name":                role,
			constants.ModelLabelEngine: "vllm",
			constants.ModelLabelPort:   port,
		}},
		Status: v1.PodStatus{
			PodIP:      host,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func Test_selectTargetPod_EPlusPD(t *testing.T) {
	const image = `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`
	withImage := `{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},` + image + `]}]}`

	tests := []struct {
		name           string
		body           string
		servingPods    int
		wantCandidates []string
		wantEncodes    int
	}{
		{name: "media is encoded on the target's roleset", body: withImage, servingPods: 2,
			wantCandidates: []string{"worker-0", "worker-1"}, wantEncodes: 1},
		{name: "text-only requests skip the encoders", body: `{"model":"qwen-vl","messages":[{"role":"user","content":"hi"}]}`,
			servingPods: 2, wantCandidates: []string{"worker-0", "worker-1"}},
		{name: "a single serving pod still gets the encode stage", body: withImage, servingPods: 1, wantEncodes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoders, otherEncoders := &encodeRecorder{}, &encodeRecorder{}
			encodeSrv, otherSrv := encoders.server(t), otherEncoders.server(t)
			workerSrv := (&encodeRecorder{}).server(t)

			pods := []*v1.Pod{epdServingPod(t, "encode-0", "rs-a", encode.Role, encodeSrv)}
			for i := 0; i < tt.servingPods; i++ {
				pods = append(pods, epdServingPod(t, "worker-"+string(rune('0'+i)), "rs-a", "worker", workerSrv))
			}
			pods = append(pods, epdServingPod(t, "encode-other", "rs-b", encode.Role, otherSrv))

			router := &firstPodRouter{}
			algo := types.RoutingAlgorithm("test-epd-" + strings.ReplaceAll(tt.name, " ", "-"))
			routing.Register(algo, func() (types.Router, error) { return router, nil })
			routing.Init()

			server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
			routeCtx := types.NewRoutingContext(context.Background(), algo, "qwen-vl", "", "epd-request", "user")
			defer routeCtx.Delete()
			routeCtx.ReqPath = "/v1/chat/completions"
			routeCtx.ReqBody = []byte(tt.body)

			_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			require.NotNil(t, routeCtx.TargetPod())
			assert.False(t, encode.IsEncoder(routeCtx.TargetPod()), "an encoder must never serve the request")
			assert.ElementsMatch(t, tt.wantCandidates, router.candidates())
			assert.Equal(t, tt.wantEncodes, encoders.count())
			assert.Zero(t, otherEncoders.count(), "encoders of another roleset must not be used")
		})
	}
}

func Test_selectTargetPod_OnlyEncodersReady(t *testing.T) {
	srv := (&encodeRecorder{}).server(t)
	algo := types.RoutingAlgorithm("test-epd-only-encoders")
	routing.Register(algo, func() (types.Router, error) { return &firstPodRouter{}, nil })
	routing.Init()

	server := &Server{}
	routeCtx := types.NewRoutingContext(context.Background(), algo, "qwen-vl", "", "epd-request", "user")
	defer routeCtx.Delete()
	pods := &utils.PodArray{Pods: []*v1.Pod{epdServingPod(t, "encode-0", "rs-a", encode.Role, srv)}}

	_, err := server.selectTargetPod(context.Background(), routeCtx, pods, "")
	assert.Error(t, err)
}

func Test_selectTargetPod_EncodersNeedARoleset(t *testing.T) {
	encoders := &encodeRecorder{}
	encodeSrv, workerSrv := encoders.server(t), (&encodeRecorder{}).server(t)
	encoder := epdServingPod(t, "encode-0", "", encode.Role, encodeSrv)
	worker := epdServingPod(t, "worker-0", "", "worker", workerSrv)
	delete(encoder.Labels, "roleset-name")
	delete(worker.Labels, "roleset-name")

	algo := types.RoutingAlgorithm("test-epd-no-roleset")
	routing.Register(algo, func() (types.Router, error) { return &firstPodRouter{}, nil })
	routing.Init()

	server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
	routeCtx := types.NewRoutingContext(context.Background(), algo, "qwen-vl", "", "epd-request", "user")
	defer routeCtx.Delete()
	routeCtx.ReqPath = "/v1/chat/completions"
	routeCtx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)

	_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: []*v1.Pod{encoder, worker}}, "")
	require.NoError(t, err)
	assert.Equal(t, "worker-0", routeCtx.TargetPod().Name)
	assert.Zero(t, encoders.count(), "pods without a roleset don't share encoders")
}

func Test_selectTargetPod_EPlusPDEngine(t *testing.T) {
	tests := []struct {
		name        string
		engine      string
		wantEncodes int
	}{
		{name: "a pod without the engine label counts as vLLM", wantEncodes: 1},
		{name: "other engines skip the encode stage", engine: "sglang"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoders := &encodeRecorder{}
			encodeSrv, workerSrv := encoders.server(t), (&encodeRecorder{}).server(t)
			pods := []*v1.Pod{
				epdServingPod(t, "encode-0", "rs-a", encode.Role, encodeSrv),
				epdServingPod(t, "worker-0", "rs-a", "worker", workerSrv),
			}
			for _, pod := range pods {
				delete(pod.Labels, constants.ModelLabelEngine)
				if tt.engine != "" {
					pod.Labels[constants.ModelLabelEngine] = tt.engine
				}
			}

			algo := types.RoutingAlgorithm("test-epd-engine-" + strings.ReplaceAll(tt.name, " ", "-"))
			routing.Register(algo, func() (types.Router, error) { return &firstPodRouter{}, nil })
			routing.Init()

			server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
			routeCtx := types.NewRoutingContext(context.Background(), algo, "qwen-vl", "", "epd-request", "user")
			defer routeCtx.Delete()
			routeCtx.ReqPath = "/v1/chat/completions"
			routeCtx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)

			_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			assert.Equal(t, "worker-0", routeCtx.TargetPod().Name)
			assert.Equal(t, tt.wantEncodes, encoders.count())
		})
	}
}

func Test_selectTargetPod_PDKeepsEncoders(t *testing.T) {
	encoders := &encodeRecorder{}
	encodeSrv, servingSrv := encoders.server(t), (&encodeRecorder{}).server(t)
	pods := []*v1.Pod{
		epdServingPod(t, "encode-0", "rs-a", encode.Role, encodeSrv),
		epdServingPod(t, "prefill-0", "rs-a", "prefill", servingSrv),
		epdServingPod(t, "decode-0", "rs-a", "decode", servingSrv),
	}
	router := &firstPodRouter{}
	routing.Register(routing.RouterPD, func() (types.Router, error) { return router, nil })
	routing.Init()

	server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "qwen-vl", "", "epd-request", "user")
	defer routeCtx.Delete()
	routeCtx.ReqPath = "/v1/chat/completions"
	routeCtx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)

	_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	// The PD router picks the encoders itself, so it gets them and the gateway
	// doesn't run the stage a second time.
	assert.ElementsMatch(t, []string{"encode-0", "prefill-0", "decode-0"}, router.candidates())
	assert.Zero(t, encoders.count())
}
