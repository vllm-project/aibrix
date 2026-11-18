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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Tests for how the pod blacklist meets disaggregated deployments and replica-inflight
// admission in selectTargetPod. The blacklist package tests ignoredPod on its own; these
// run it through the routing path, which is where a PD request, an encoder or an admission
// would actually be affected.

// The PD strategy filters the whole ready list, so a prefill, decode or encode pod that the
// blacklist tracked would be dropped from the roleset the PD router pairs. They are never
// tracked, whoever created them: the pods here carry only the role label. plain-0, a pod of
// no PD role, is the control that shows the filter does run on this path.
func Test_selectTargetPod_PDRolePodsAreNeverExcluded(t *testing.T) {
	encoders := &encodeRecorder{}
	encodeSrv, servingSrv := encoders.server(t), (&encodeRecorder{}).server(t)
	roles := []*v1.Pod{
		epdServingPod(t, "encode-0", "rs-a", encode.Role, encodeSrv),
		epdServingPod(t, "prefill-0", "rs-a", "prefill", servingSrv),
		epdServingPod(t, "decode-0", "rs-a", "decode", servingSrv),
	}
	plain := epdServingPod(t, "plain-0", "rs-a", "worker", servingSrv)
	pods := append(append([]*v1.Pod{}, roles...), plain)

	server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
	for _, pod := range pods {
		tripBreaker(&server.podBlacklist, pod)
	}
	for _, pod := range roles {
		require.False(t, server.podBlacklist.IsBlacklisted(pod), "%s is a PD role pod, so no verdict can exclude it", pod.Name)
	}
	require.True(t, server.podBlacklist.IsBlacklisted(plain), "the control pod is tracked, so it is excluded")

	router := &firstPodRouter{}
	routing.Register(routing.RouterPD, func() (types.Router, error) { return router, nil })
	routing.Init()
	routeCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, "qwen-vl", "", "pd-blacklist-request", "user")
	defer routeCtx.Delete()

	_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"encode-0", "prefill-0", "decode-0"}, router.candidates(),
		"the PD router gets every role pod, and only the tracked control pod is dropped")
}

// For a strategy other than PD the encoders are split off before the blacklist filter and
// handed to the encode stage as they are. A verdict recorded against one has no effect, and
// the serving pods are still filtered: worker-1 is excluded and never reaches the router.
func Test_selectTargetPod_EPlusPDWithBlacklist(t *testing.T) {
	const image = `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`
	body := `{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},` + image + `]}]}`

	encoders := &encodeRecorder{}
	encodeSrv, workerSrv := encoders.server(t), (&encodeRecorder{}).server(t)
	encoder := epdServingPod(t, "encode-0", "rs-a", encode.Role, encodeSrv)
	pods := []*v1.Pod{
		encoder,
		epdServingPod(t, "worker-0", "rs-a", "worker", workerSrv),
		epdServingPod(t, "worker-1", "rs-a", "worker", workerSrv),
		epdServingPod(t, "worker-2", "rs-a", "worker", workerSrv),
	}

	server := &Server{encodeExecutor: encode.NewExecutor(encodeSrv.Client())}
	tripBreaker(&server.podBlacklist, pods[2])
	tripBreaker(&server.podBlacklist, encoder)
	require.True(t, server.podBlacklist.IsBlacklisted(pods[2]))
	require.False(t, server.podBlacklist.IsBlacklisted(encoder), "an encoder is never tracked")

	router := &firstPodRouter{}
	algo := types.RoutingAlgorithm("test-epd-blacklist")
	routing.Register(algo, func() (types.Router, error) { return router, nil })
	routing.Init()
	routeCtx := types.NewRoutingContext(context.Background(), algo, "qwen-vl", "", "epd-blacklist-request", "user")
	defer routeCtx.Delete()
	routeCtx.ReqPath = "/v1/chat/completions"
	routeCtx.ReqBody = []byte(body)

	_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"worker-0", "worker-2"}, router.candidates(), "the excluded worker is dropped, and no encoder is a candidate")
	require.NotNil(t, routeCtx.TargetPod())
	assert.False(t, encode.IsEncoder(routeCtx.TargetPod()))
	assert.Equal(t, 1, encoders.count(), "the encode stage still runs on the encoder")
}

// A pod can be excluded between the filter and the router's pick, since the sweep runs
// concurrently. The replica-inflight admission does not re-check the blacklist: it can only
// fail the request, and a soft preference must not fail one that has not itself gone wrong
// (see the README's known limitations). This pins that: the request is admitted and routed to
// the pod, not rejected.
func Test_selectTargetPod_PodExcludedAfterTheFilterIsStillAdmitted(t *testing.T) {
	const limit = int64(5)
	mockCache := &MockCache{}
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{}, nil)
	mockCache.On("AdmitPodRunningRequest", mock.Anything, "ns", limit).Return(true, true, nil).Once()
	server := &Server{cache: mockCache}

	var picked *v1.Pod
	router := routeFunc(func(ctx *types.RoutingContext, pods types.PodList) (string, error) {
		picked = pods.All()[0]
		// The sweep excludes the pod after the filter has run and before the router admits it.
		tripBreaker(&server.podBlacklist, picked)
		if !server.podBlacklist.IsBlacklisted(picked) {
			t.Errorf("setup: %s should be excluded by now", picked.Name)
		}
		if err := ctx.AdmitTarget(picked); err != nil {
			return "", err
		}
		ctx.SetTargetPod(picked)
		return ctx.TargetAddress(), nil
	})
	algo := types.RoutingAlgorithm("test-blacklist-admission-router")
	routing.Register(algo, func() (types.Router, error) { return router, nil })
	routing.Init()

	routeCtx := types.NewRoutingContext(context.Background(), algo, "test-model", "", "admission-request", "user")
	defer routeCtx.Delete()
	routeCtx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: limit}
	pods := []*v1.Pod{blacklistTestPod("a"), blacklistTestPod("b")}

	addr, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")

	require.NoError(t, err, "admission does not fail the request because the pod became excluded")
	require.NotNil(t, picked)
	assert.Equal(t, picked, routeCtx.TargetPod())
	assert.NotEmpty(t, addr)
	assert.True(t, server.podBlacklist.IsBlacklisted(picked), "the pod is excluded now, and was still routed to")
	mockCache.AssertExpectations(t)
}
