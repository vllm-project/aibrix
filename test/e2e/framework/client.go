/*
Copyright 2024 The Aibrix Team.

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
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	ModelName           = "llama2-7b"
	ModelNameQwen3      = "qwen3-8b"
	ModelNameVLLM       = "llama2-7b-vllm"
	ModelNameVLLMBucket = "llama2-7b-vllm-bucket"
	ModelNameSGLang     = "llama2-7b-sglang"
	ModelNameTRTLLM     = "llama2-7b-trtllm"

	// config/test runs two gateway-plugin replicas. Each has its own pod cache, so one
	// successful PD probe is not enough after pod churn — Envoy may send the next
	// request to the replica that has not seen the update yet.
	pdRoutingConsecutiveSuccesses = 3
	pdRoutingWaitTimeout          = 2 * time.Minute
	pdChatRetryTimeout            = 30 * time.Second
	pdChatRetryInterval           = 1 * time.Second
)

func InitializeClient(ctx context.Context, t *testing.T) (*kubernetes.Clientset, *v1alpha1.Clientset) {
	var err error
	var config *rest.Config

	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig == "" {
		t.Error("kubeConfig not set")
	}
	t.Logf("using configuration from '%s'\n", kubeConfig)

	config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		t.Errorf("Error during client creation with %v\n", err)
	}
	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Errorf("Error during client creation with %v\n", err)
	}
	crdClientSet, err := v1alpha1.NewForConfig(config)
	if err != nil {
		t.Errorf("Error during client creation with %v\n", err)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

	defer runtime.HandleCrash()
	factory.Start(ctx.Done())
	crdFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced, modelInformer.HasSynced) {
		t.Error("timed out waiting for caches to sync")
	}

	return k8sClientSet, crdClientSet
}

func NewOpenAIClient(baseURL, apiKey string) openai.Client {
	// For strict testing, use a custom http.Transport with disabled keep-alives and caching to avoid flaky tests.
	transport := &http.Transport{
		DisableKeepAlives: true,
		MaxIdleConns:      0,
	}

	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Transport: transport}),
		option.WithMiddleware(func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
			r.URL.Path = "/v1" + r.URL.Path
			return mn(r)
		}),
		option.WithMaxRetries(0),
	)
}

func NewOpenAIClientWithRoutingStrategy(baseURL, apiKey, routingStrategy string,
	respOpt option.RequestOption) openai.Client {
	// For strict testing, use a custom http.Transport with disabled keep-alives and caching to avoid flaky tests.
	transport := &http.Transport{
		DisableKeepAlives: true,
		MaxIdleConns:      0,
	}

	return openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Transport: transport}),
		option.WithMiddleware(func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
			r.URL.Path = "/v1" + r.URL.Path
			return mn(r)
		}),
		option.WithHeader("routing-strategy", routingStrategy),
		option.WithMaxRetries(0),
		respOpt,
	)
}

// NewOpenAIClientWithConfigProfile creates a client that sends config-profile header.
// The gateway plugin selects routing-strategy from the model's config profile (model.aibrix.ai/config)
// based on this header, rather than from the routing-strategy header.
func NewOpenAIClientWithConfigProfile(baseURL, apiKey, configProfile string,
	respOpts ...option.RequestOption) openai.Client {
	transport := &http.Transport{
		DisableKeepAlives: true,
		MaxIdleConns:      0,
	}

	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{Transport: transport}),
		option.WithMiddleware(func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
			r.URL.Path = "/v1" + r.URL.Path
			return mn(r)
		}),
		option.WithMaxRetries(0),
	}
	if configProfile != "" {
		opts = append(opts, option.WithHeader("config-profile", configProfile))
	}
	for _, respOpt := range respOpts {
		if respOpt != nil {
			opts = append(opts, respOpt)
		}
	}

	return openai.NewClient(opts...)
}

func ValidateInference(t *testing.T, modelName string) {
	config := LoadConfig()
	client := NewOpenAIClient(config.GatewayURL, config.APIKey)
	ValidateInferenceWithClient(t, client, modelName)
}

func ValidateInferenceWithClient(t *testing.T, client openai.Client, modelName string) {
	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Say this is a test"),
		},
		Model: modelName,
	})
	if err != nil {
		t.Fatalf("chat completions failed : %v", err)
	}
	assert.Equal(t, modelName, chatCompletion.Model)
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion has no choices returned")
	assert.NotNil(t, chatCompletion.Choices[0].Message.Content, "chat completion has no message returned")
}

// ValidateAllPodsAreReady waits until the default namespace has exactly expectedPodCount
// active pods. An optional labelSelector scopes the count to a subset of pods, which is
// necessary when other, unrelated workloads share the namespace.
func ValidateAllPodsAreReady(t *testing.T, client *kubernetes.Clientset, expectedPodCount int,
	labelSelector ...string) {
	selector := ""
	if len(labelSelector) > 0 {
		selector = labelSelector[0]
	}
	namespace := LoadConfig().Namespace
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, 2*time.Minute,
		true, func(ctx context.Context) (bool, error) {
			podList, err := client.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{LabelSelector: selector})
			if err != nil {
				t.Logf("failed to list pods: %v", err)
				return false, err
			}
			activePods := utils.FilterActivePods(podList.Items)
			if len(activePods) == expectedPodCount {
				t.Logf("All %d pods are ready", expectedPodCount)
				return true, nil
			}
			t.Logf("Waiting for %d pods to be ready. Current count: %d", expectedPodCount, len(activePods))
			return false, nil
		})
	require.NoError(t, err, "timeout waiting for all pods to be ready")
}

// PollPDChatCompletion retries a PD chat completion until it succeeds or timeout.
// A single 503 after pod churn is not treated as failure: Envoy may still be
// hitting a gateway-plugin replica whose pod cache has not caught up.
func PollPDChatCompletion(
	t *testing.T, client openai.Client, params openai.ChatCompletionNewParams,
) *openai.ChatCompletion {
	t.Helper()
	var last *openai.ChatCompletion
	var lastErr error
	err := wait.PollUntilContextTimeout(context.Background(), pdChatRetryInterval, pdChatRetryTimeout, true,
		func(ctx context.Context) (bool, error) {
			resp, err := client.Chat.Completions.New(ctx, params)
			if err != nil {
				lastErr = err
				t.Logf("retrying PD chat completion: %v", err)
				return false, nil
			}
			last = resp
			lastErr = nil
			return true, nil
		})
	if lastErr != nil {
		require.NoError(t, err, "timeout waiting for PD chat completion: %v", lastErr)
	}
	require.NoError(t, err, "timeout waiting for PD chat completion")
	return last
}

func waitForConsecutivePDSuccesses(
	t *testing.T, modelName string, attempt func(ctx context.Context) (ok bool, msg string),
) {
	t.Helper()
	consecutive := 0
	err := wait.PollUntilContextTimeout(context.Background(), pdChatRetryInterval, pdRoutingWaitTimeout, true,
		func(ctx context.Context) (bool, error) {
			ok, msg := attempt(ctx)
			if !ok {
				consecutive = 0
				t.Logf("%s", msg)
				return false, nil
			}
			consecutive++
			if consecutive < pdRoutingConsecutiveSuccesses {
				t.Logf("%s (%d/%d consecutive)", msg, consecutive, pdRoutingConsecutiveSuccesses)
				return false, nil
			}
			t.Logf("%s", msg)
			return true, nil
		})
	require.NoError(t, err, "timeout waiting for PD routing to be ready for model %s", modelName)
}

// WaitForPDDisaggregationRouting polls until the gateway can route a PD request for modelName.
// Pod readiness alone is not enough: the gateway pod cache may lag after pod churn, and
// with two plugin replicas one success can be a lucky hit on the warm replica.
func WaitForPDDisaggregationRouting(t *testing.T, modelName string) {
	t.Helper()
	var dst *http.Response
	config := LoadConfig()
	client := NewOpenAIClientWithRoutingStrategy(config.GatewayURL, config.APIKey, "pd", option.WithResponseInto(&dst))

	waitForConsecutivePDSuccesses(t, modelName, func(ctx context.Context) (bool, string) {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage("PD routing readiness check"),
			},
			Model: modelName,
		})
		if err != nil {
			return false, "waiting for PD routing for model " + modelName + ": " + err.Error()
		}
		prefillPod := dst.Header.Get("prefill-target-pod")
		decodePod := dst.Header.Get("target-pod")
		if prefillPod == "" || decodePod == "" || prefillPod == decodePod {
			return false, "waiting for valid PD routing headers for model " + modelName
		}
		return true, "PD routing ready for model " + modelName
	})
}

// WaitForPDCombinedRouting polls until the gateway routes a long prompt to a combined pod
// (no prefill-target-pod header). Pod cache may lag behind Kubernetes readiness.
func WaitForPDCombinedRouting(t *testing.T, modelName, combinedStormName, longPrompt string) {
	t.Helper()
	var dst *http.Response
	config := LoadConfig()
	client := NewOpenAIClientWithRoutingStrategy(config.GatewayURL, config.APIKey, "pd", option.WithResponseInto(&dst))

	waitForConsecutivePDSuccesses(t, modelName, func(ctx context.Context) (bool, string) {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage(longPrompt),
			},
			Model: modelName,
		})
		if err != nil {
			return false, "waiting for combined PD routing for model " + modelName + ": " + err.Error()
		}
		prefillPod := dst.Header.Get("prefill-target-pod")
		decodePod := dst.Header.Get("target-pod")
		if prefillPod != "" || decodePod == "" || !strings.Contains(decodePod, combinedStormName) {
			return false, "waiting for combined routing for model " + modelName +
				": prefill=" + prefillPod + " decode=" + decodePod
		}
		return true, "combined PD routing ready for model " + modelName + " (decode=" + decodePod + ")"
	})
}
