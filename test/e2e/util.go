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

package e2e

import (
	"context"
	"net/http"
	"os"
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

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	gatewayURL     = "http://localhost:8888"
	engineURL      = "http://localhost:8000"
	apiKey         = "test-key-1234567890"
	modelName      = "llama2-7b"
	modelNameQwen3 = "qwen3-8b"
	namespace      = "aibrix-system"
)

func initializeClient(ctx context.Context, t *testing.T) (*kubernetes.Clientset, *v1alpha1.Clientset) {
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

func createOpenAIClient(baseURL, apiKey string) openai.Client {
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

func createOpenAIClientWithRoutingStrategy(baseURL, apiKey, routingStrategy string,
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

// createOpenAIClientWithConfigProfile creates a client that sends config-profile header.
// The gateway plugin selects routing-strategy from the model's config profile (model.aibrix.ai/config)
// based on this header, rather than from the routing-strategy header.
func createOpenAIClientWithConfigProfile(baseURL, apiKey, configProfile string,
	respOpt option.RequestOption) openai.Client {
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
	if respOpt != nil {
		opts = append(opts, respOpt)
	}

	return openai.NewClient(opts...)
}

func validateInference(t *testing.T, modelName string) {
	client := createOpenAIClient(gatewayURL, apiKey)
	validateInferenceWithClient(t, client, modelName)
}

func validateInferenceWithClient(t *testing.T, client openai.Client, modelName string) {
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

func validateAllPodsAreReady(t *testing.T, client *kubernetes.Clientset, expectedPodCount int) {
	err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second,
		true, func(ctx context.Context) (bool, error) {
			podList, err := client.CoreV1().Pods("default").List(ctx, v1.ListOptions{})
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
	assert.NoError(t, err, "timeout waiting for all pods to be ready")
}
