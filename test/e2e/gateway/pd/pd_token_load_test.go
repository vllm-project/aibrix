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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

const (
	// tokenLoadConfigProfile is the model.aibrix.ai/config profile declared in
	// development/app/config/mock/vllm-pd-config.yaml that selects
	// routingConfig.prefillScorePolicy=token_load for the llama2-7b-vllm PD model.
	tokenLoadConfigProfile = "token-load"

	gatewayPluginsLabelSelector = "app=gateway-plugins"
	gatewayPluginsMetricsPort   = "8080"
	gatewayPluginsMetricsPath   = "metrics"
)

// tokenLoadGauge is one pd_token_load_* series scraped from one gateway-plugins pod.
type tokenLoadGauge struct {
	gateway string
	metric  string
	podName string
	value   float64
}

func (g tokenLoadGauge) String() string {
	return fmt.Sprintf("%s{pod_name=%q}=%g on %s", g.metric, g.podName, g.value, g.gateway)
}

// listGatewayPluginPods returns the running gateway-plugins pods. The e2e
// deployment runs more than one replica and every replica keeps its own
// token-load ledger, so metrics have to be scraped from all of them.
func listGatewayPluginPods(t *testing.T, ctx context.Context, k8sClient *kubernetes.Clientset) []corev1.Pod {
	t.Helper()
	pods, err := k8sClient.CoreV1().Pods(gatewayNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: gatewayPluginsLabelSelector,
	})
	require.NoError(t, err)
	running := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod)
		}
	}
	require.NotEmpty(t, running, "no running gateway-plugins pods found in namespace %s", gatewayNamespace)
	return running
}

// scrapeTokenLoadGauges reads /metrics from every gateway-plugins pod through
// the API server proxy and returns the pd_token_load_active_tokens and
// pd_token_load_kv_tokens series it finds.
func scrapeTokenLoadGauges(
	ctx context.Context, k8sClient *kubernetes.Clientset, gatewayPods []corev1.Pod,
) ([]tokenLoadGauge, error) {
	var gauges []tokenLoadGauge
	for _, gw := range gatewayPods {
		raw, err := k8sClient.CoreV1().Pods(gatewayNamespace).ProxyGet(
			"http", gw.Name, gatewayPluginsMetricsPort, gatewayPluginsMetricsPath, nil,
		).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("scrape %s: %w", gw.Name, err)
		}
		var parser expfmt.TextParser
		families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse metrics from %s: %w", gw.Name, err)
		}
		for _, name := range []string{metrics.PDTokenLoadActiveTokens, metrics.PDTokenLoadKVTokens} {
			family, ok := families[name]
			if !ok {
				continue
			}
			for _, m := range family.GetMetric() {
				g := tokenLoadGauge{gateway: gw.Name, metric: name, value: m.GetGauge().GetValue()}
				for _, label := range m.GetLabel() {
					if label.GetName() == "pod_name" {
						g.podName = label.GetValue()
					}
				}
				gauges = append(gauges, g)
			}
		}
	}
	return gauges, nil
}

// TestPDDisaggregationVLLMTokenLoad verifies the token_load prefill score policy
// end to end: selecting it through a model config profile keeps PD routing
// working, every prefill pod it routes to is charged in the gateway's token
// ledger (visible as pd_token_load_* gauges labelled with that pod), and the
// charges are released once the requests complete, so nothing leaks.
//
// The mock engines answer in milliseconds and the e2e gateway runs several
// replicas with independent ledgers, so the load-steering behaviour itself is
// covered by the router unit tests; this test pins the wiring.
func TestPDDisaggregationVLLMTokenLoad(t *testing.T) {
	const iterations = 6
	ctx := context.Background()

	waitForPDDisaggregationRouting(t, modelNameVLLM)

	k8sClient, _ := initializeClient(ctx, t)
	gatewayPods := listGatewayPluginPods(t, ctx, k8sClient)

	longPrompt := strings.Repeat("token load e2e prompt ", 300)
	shortPrompt := "token load e2e short prompt"

	var dst *http.Response
	client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, tokenLoadConfigProfile,
		option.WithResponseInto(&dst))

	prefillPods := map[string]struct{}{}
	for i := 0; i < iterations; i++ {
		prompt := shortPrompt
		if i%2 == 0 {
			prompt = longPrompt
		}
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
			Model:    modelNameVLLM,
		})
		require.NoError(t, err, "token_load PD chat completion request %d failed", i)

		assert.Equal(t, "pd", dst.Header.Get("routing-strategy"),
			"request %d: config profile %q should resolve to PD routing", i, tokenLoadConfigProfile)
		prefillPod := dst.Header.Get("prefill-target-pod")
		decodePod := dst.Header.Get("target-pod")
		require.NotEmpty(t, prefillPod, "request %d: prefill-target-pod header must be set", i)
		require.NotEmpty(t, decodePod, "request %d: target-pod header must be set", i)
		assert.NotEqual(t, prefillPod, decodePod, "request %d: prefill and decode pods should differ", i)
		prefillPods[prefillPod] = struct{}{}
		t.Logf("request %d — prefill: %s, decode: %s", i, prefillPod, decodePod)
	}

	// Every prefill pod that served a request must have been charged on the
	// gateway replica that routed it, and all charges must be back at zero
	// once the responses have been delivered.
	var lastGauges []tokenLoadGauge
	err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			gauges, err := scrapeTokenLoadGauges(ctx, k8sClient, gatewayPods)
			if err != nil {
				t.Logf("waiting for gateway metrics: %v", err)
				return false, nil
			}
			lastGauges = gauges

			charged := map[string]bool{}
			settled := true
			for _, g := range gauges {
				if _, ok := prefillPods[g.podName]; !ok {
					continue
				}
				if g.metric == metrics.PDTokenLoadKVTokens {
					charged[g.podName] = true
				}
				if g.value != 0 {
					settled = false
				}
			}
			if len(charged) != len(prefillPods) || !settled {
				t.Logf("waiting for token_load gauges (charged %d/%d prefill pods, settled=%v)",
					len(charged), len(prefillPods), settled)
				return false, nil
			}
			return true, nil
		})
	require.NoError(t, err, "token_load gauges did not settle; last scrape: %v", lastGauges)

	for podName := range prefillPods {
		found := false
		for _, g := range lastGauges {
			if g.podName != podName {
				continue
			}
			if g.metric == metrics.PDTokenLoadKVTokens {
				found = true
			}
			assert.Zero(t, g.value, "%s should be released after the request completed", g)
		}
		assert.True(t, found, "prefill pod %s served a token_load request but no %s series was published for it",
			podName, metrics.PDTokenLoadKVTokens)
	}
	t.Logf("token_load gauges after %d requests: %v", iterations, lastGauges)
}
