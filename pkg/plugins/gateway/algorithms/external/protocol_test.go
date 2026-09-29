/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package external

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type externalProtocolCache struct {
	cache.Cache
	runningCalls int
	running      map[string]int64
	modelMetrics map[string]float64
}

func (c *externalProtocolCache) RegisterRequestTracker(cache.RequestTracker) {}

func TestExternalOpenAPIFixtures(t *testing.T) {
	snapshots := map[string]externalCandidateSnapshot{
		"default/llama-3-a": {
			pod:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "llama-3-a"}},
			ports: map[int]struct{}{8000: {}},
		},
	}
	for _, tt := range []struct {
		name     string
		mode     externalPolicyMode
		decision string
	}{
		{name: "selected.json", mode: PolicyAdvisory, decision: externalDecisionSelected},
		{name: "no-decision.json", mode: PolicyAdvisory, decision: externalDecisionNoDecision},
		{name: "denied.json", mode: PolicyAuthoritative, decision: externalDecisionDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + tt.name)
			require.NoError(t, err)
			decision, err := validateExternalDecision(body, "req-123", tt.mode, snapshots)
			require.NoError(t, err)
			require.Equal(t, tt.decision, decision.decision)
		})
	}
}

func (c *externalProtocolCache) GetPodsRunningRequests(_ []*v1.Pod) (map[string]int64, error) {
	c.runningCalls++
	return c.running, nil
}

func (c *externalProtocolCache) GetMetricValueByPodModel(podName, _, _, metric string) (metrics.MetricValue, error) {
	value, ok := c.modelMetrics[podName+"/"+metric]
	if !ok {
		return nil, context.Canceled
	}
	return &metrics.SimpleMetricValue{Value: value}, nil
}

func externalTestPod(namespace, name, ip, zone string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				constants.ModelLabelPort:      "8000",
				"topology.kubernetes.io/zone": zone,
				"private":                     "do-not-send",
			},
		},
		Status: v1.PodStatus{PodIP: ip},
	}
}

func TestBuildExternalDecisionRequest(t *testing.T) {
	podB := externalTestPod("default", "b", "10.0.0.2", "zone-b")
	podA := externalTestPod("default", "a", "10.0.0.1", "zone-a")
	metricCache := &externalProtocolCache{
		running: map[string]int64{
			utils.GeneratePodKey("default", "a"): 3,
			utils.GeneratePodKey("default", "b"): 5,
		},
		modelMetrics: map[string]float64{
			"a/" + metrics.EngineUtilization: 0.5,
			"a/" + metrics.KVCacheUsagePerc:  0.25,
			"b/" + metrics.EngineUtilization: math.NaN(),
			"b/" + metrics.KVCacheUsagePerc:  2,
		},
	}
	cfg := externalRouterConfig{
		policyMode:          PolicyAdvisory,
		candidateAttributes: []string{"topology.kubernetes.io/zone"},
		policyAttributes:    []string{"tenantTier"},
		candidateMetrics: map[string]struct{}{
			externalMetricRunningRequests:   {},
			externalMetricEngineUtilization: {},
			externalMetricKVCacheUsage:      {},
		},
	}
	ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "secret prompt", "req-1", "")
	ctx.ReqBody = []byte("{\"authorization\":\"secret\"}")
	ctx.ReqHeaders["authorization"] = "Bearer secret"
	ctx.SetTrustedPolicyAttribute("tenantTier", "gold")
	ctx.SetTrustedPolicyAttribute("notAllowed", "hidden")

	request, snapshots, err := buildExternalDecisionRequest(cfg, metricCache, ctx, &utils.PodArray{Pods: []*v1.Pod{podB, podA}})
	require.NoError(t, err)
	require.Equal(t, 1, metricCache.runningCalls)
	require.Equal(t, []string{"default/a", "default/b"}, []string{request.Spec.Candidates[0].ID, request.Spec.Candidates[1].ID})
	require.Equal(t, map[string]string{"tenantTier": "gold"}, request.Spec.PolicyContext.Attributes)
	require.Equal(t, int64(3), *request.Spec.Candidates[0].Metrics.RunningRequests)
	require.Equal(t, 0.5, *request.Spec.Candidates[0].Metrics.EngineUtilization)
	require.Equal(t, 0.25, *request.Spec.Candidates[0].Metrics.KVCacheUsage)
	require.Nil(t, request.Spec.Candidates[1].Metrics.EngineUtilization)
	require.Nil(t, request.Spec.Candidates[1].Metrics.KVCacheUsage)
	require.Len(t, snapshots, 2)

	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	body := string(encoded)
	require.NotContains(t, body, "10.0.0")
	require.NotContains(t, body, "secret prompt")
	require.NotContains(t, body, "authorization")
	require.NotContains(t, body, "private")
	require.NotContains(t, body, "notAllowed")
}

func TestBuildExternalDecisionRequestKeepsNamespacePortsSeparate(t *testing.T) {
	podA := externalTestPod("ns-a", "same-name", "10.0.0.1", "zone-a")
	podB := externalTestPod("ns-b", "same-name", "10.0.0.2", "zone-b")
	podA.Labels[constants.ModelLabelPort] = "8000"
	podB.Labels[constants.ModelLabelPort] = "9000"
	cfg := externalRouterConfig{policyMode: PolicyAuthoritative, candidateMetrics: map[string]struct{}{}}
	ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "same-name", "")

	request, snapshots, err := buildExternalDecisionRequest(cfg, nil, ctx, &utils.PodArray{Pods: []*v1.Pod{podB, podA}})
	require.NoError(t, err)
	require.Equal(t, []int{8000}, request.Spec.Candidates[0].Ports)
	require.Equal(t, []int{9000}, request.Spec.Candidates[1].Ports)
	_, hasA8000 := snapshots["ns-a/same-name"].ports[8000]
	_, hasA9000 := snapshots["ns-a/same-name"].ports[9000]
	require.True(t, hasA8000)
	require.False(t, hasA9000)
}

func TestFilterExternalAttributesDeterministicallyLimitsEntries(t *testing.T) {
	input := make(map[string]string, 33)
	for i := 32; i >= 0; i-- {
		input[fmt.Sprintf("key%02d", i)] = "value"
	}

	filtered := filterExternalAttributes(input)
	require.Len(t, filtered, 32)
	require.Contains(t, filtered, "key00")
	require.NotContains(t, filtered, "key32")
}

func TestValidateExternalDecision(t *testing.T) {
	snapshots := map[string]externalCandidateSnapshot{
		"default/a": {
			pod:   &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "a"}},
			ports: map[int]struct{}{8000: {}},
		},
	}
	selected := "{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":\"req-1\",\"decisionId\":\"d1\"},\"status\":{\"decision\":\"Selected\",\"target\":{\"id\":\"default/a\"}}}"
	decision, err := validateExternalDecision([]byte(selected), "req-1", PolicyAdvisory, snapshots)
	require.NoError(t, err)
	require.Equal(t, 8000, decision.targetPort)
	require.Equal(t, "a", decision.targetPod.Name)

	noDecision := "{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":\"req-1\"},\"status\":{\"decision\":\"NoDecision\",\"reason\":\"none\"},\"future\":{\"ok\":true}}"
	_, err = validateExternalDecision([]byte(noDecision), "req-1", PolicyAdvisory, snapshots)
	require.NoError(t, err)

	denied := "{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":\"req-1\"},\"status\":{\"decision\":\"Denied\",\"reason\":\"policy\"}}"
	_, err = validateExternalDecision([]byte(denied), "req-1", PolicyAuthoritative, snapshots)
	require.NoError(t, err)

	invalid := []string{
		strings.Replace(selected, "\"req-1\"", "\"wrong\"", 1),
		strings.Replace(selected, "\"default/a\"", "\"default/b\"", 1),
		strings.Replace(selected, "\"Selected\"", "\"Unknown\"", 1),
		"{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":\"req-1\"},\"status\":{\"decision\":\"NoDecision\"}}",
		selected + " {}",
		denied,
		"{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":\"req-1\",\"decisionId\":null},\"status\":{\"decision\":\"NoDecision\"}}",
	}
	for i, raw := range invalid {
		_, err := validateExternalDecision([]byte(raw), "req-1", PolicyAdvisory, snapshots)
		require.Error(t, err, "case %d", i)
	}
}
