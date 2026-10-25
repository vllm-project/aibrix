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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/internal/trafficdraintest"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	controllerconst "github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/controller/roleset"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type trafficBeforeDeleteClient struct {
	client.Client
	before func()
}

func (c *trafficBeforeDeleteClient) Delete(ctx context.Context, pod client.Object, opts ...client.DeleteOption) error {
	c.before()
	return c.Client.Delete(ctx, pod, opts...)
}

// This test deliberately uses only pre-existing API entry points and literal
// annotation names, so it can also run against the prior best-effort patch.
func TestTrafficDrainStaleGatewayCannotEnterDeletionWindow(t *testing.T) {
	ctx := context.Background()
	mr, redisClient, _ := trafficdraintest.NewRedis(t)
	host, port, err := net.SplitHostPort(mr.Addr())
	require.NoError(t, err)
	t.Setenv("REDIS_HOST", host)
	t.Setenv("REDIS_PORT", port)
	t.Setenv("REDIS_PASSWORD", "")
	metricsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"test-model\"} 0\n# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"test-model\"} 0\n")
	}))
	defer metricsServer.Close()
	metricHost, metricPort, err := net.SplitHostPort(strings.TrimPrefix(metricsServer.URL, "http://"))
	require.NoError(t, err)
	epoch := strings.Repeat("a", 40) + ":" + strings.Repeat("1", 32)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replica", Namespace: "ns", UID: "replica-uid", Labels: map[string]string{
		constants.ModelLabelName: "test-model", constants.ModelLabelMetricPort: metricPort, controllerconst.RoleSetNameLabelKey: "owner", controllerconst.RoleNameLabelKey: "worker",
	}, Annotations: map[string]string{"aibrix.ai/traffic-scale-in-max-wait": "30s", "aibrix.ai/traffic-drain-protocol": "v1", "aibrix.ai/traffic-drain-epoch": epoch}}, Status: v1.PodStatus{PodIP: metricHost, Phase: v1.PodRunning, Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}}
	// Gateway B's cache stays stale throughout the test.
	stale := pod.DeepCopy()
	pod.Annotations[constants.PodDrainingAnnotationKey] = "true"
	pod.Annotations[constants.PodDrainStartTimeAnnotationKey] = time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339)
	pod.Annotations[constants.PodDrainReasonAnnotationKey] = constants.PodDrainReasonScaleIn
	pod.Annotations[constants.PodDrainTargetActionAnnotationKey] = constants.PodDrainTargetActionDelete
	require.NoError(t, redisClient.HSet(ctx, "aibrix:traffic-drain:{replica-uid}", map[string]interface{}{"epoch": epoch, "phase": 1, "active": 0, "generation": 0, "deadline": 0, "retry": 0}).Err())
	localCache := cache.NewWithPodsForTest([]*v1.Pod{stale}, "test-model")
	gateway := NewServerWithOptions(redisClient, nil, nil, ServerOptions{Cache: localCache, DisableRateLimiting: true})
	t.Cleanup(gateway.Shutdown)
	request := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)}}}
	rc := types.NewRoutingContext(ctx, "", "", "", "late-request", "user")
	defer rc.Delete()
	rc.ReqPath = PathChatCompletions
	rc.ReqHeaders[HeaderRoutingStrategy] = "least-request"
	var admission *extProcPb.ProcessingResponse
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	owner := &orchestrationv1alpha1.RoleSet{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns"}}
	role := &orchestrationv1alpha1.RoleSpec{Name: "worker", Replicas: ptr.To[int32](0), Drain: &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](1)}, Template: v1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"aibrix.ai/traffic-scale-in-max-wait": "30s"}}}, UpdateStrategy: orchestrationv1alpha1.RoleUpdateStrategy{MaxUnavailable: ptr.To(intstr.FromInt(1))}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()
	called := false
	cli := &trafficBeforeDeleteClient{Client: base, before: func() {
		called = true
		admission, _, _, _ = gateway.HandleRequestBody(ctx, rc, "late-request", request, utils.User{Name: "user"})
	}}
	_, err = roleset.GetRoleSyncerWithRecorder(cli, role, nil).Scale(ctx, owner, role)
	require.NoError(t, err)
	require.True(t, called)
	require.NotNil(t, admission.GetImmediateResponse(), "a stale Gateway must not admit after the idle observation and before deletion")
	require.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, admission.GetImmediateResponse().GetStatus().GetCode())
}
