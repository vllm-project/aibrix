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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/require"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const pdRequestTimeout = 30 * time.Second

// PDRequestResult contains the raw HTTP response from a PD gateway request.
type PDRequestResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	RequestID  string
}

// NewRequestID returns a request ID suitable for correlating gateway and backend records.
func NewRequestID(prefix string) string {
	if prefix == "" {
		prefix = "request"
	}
	return prefix + "-" + uuid.New().String()
}

// SendPDRequest sends an already-serialized JSON request through the gateway.
func SendPDRequest(
	ctx context.Context,
	config Config,
	routingStrategy, requestID string,
	body []byte,
) (PDRequestResult, error) {
	return SendPDRequestWithHeaders(ctx, config, routingStrategy, requestID, body, nil)
}

// SendPDRequestWithHeaders sends an already-serialized JSON request with
// additional request-scoped headers used by mock PD contract tests.
func SendPDRequestWithHeaders(
	ctx context.Context,
	config Config,
	routingStrategy, requestID string,
	body []byte,
	extraHeaders http.Header,
) (PDRequestResult, error) {
	result := PDRequestResult{RequestID: requestID}
	url := strings.TrimRight(config.GatewayURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("routing-strategy", routingStrategy)
	req.Header.Set("x-request-id", requestID)
	for key, values := range extraHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := (&http.Client{Timeout: pdRequestTimeout}).Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()

	result.StatusCode = resp.StatusCode
	result.Headers = resp.Header.Clone()
	result.Body, err = io.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf(
			"PD request failed with status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(result.Body)),
		)
	}
	return result, nil
}

const (
	ModelName                     = "llama2-7b"
	ModelNameQwen3                = "qwen3-8b"
	ModelNameQwen3ReplicaRPS      = "qwen3-8b-replica-rps"
	ModelNameQwen3ReplicaInflight = "qwen3-8b-replica-inflight"
	ModelNameVLLM                 = "llama2-7b-vllm"
	ModelNameVLLMBucket           = "llama2-7b-vllm-bucket"
	ModelNameSGLang               = "llama2-7b-sglang"
	ModelNameTRTLLM               = "llama2-7b-trtllm"

	// config/test runs two gateway-plugin replicas. Each has its own pod cache, so one
	// successful PD probe is not enough after pod churn — Envoy may send the next
	// request to the replica that has not seen the update yet.
	pdRoutingConsecutiveSuccesses = 3
	pdRoutingWaitTimeout          = 2 * time.Minute
	pdChatRetryTimeout            = 30 * time.Second
	pdChatRetryInterval           = 1 * time.Second
)

func buildKubernetesConfig(t *testing.T) *rest.Config {
	kubeConfig := os.Getenv("KUBECONFIG")
	config, err := loadKubernetesConfig(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("using configuration from '%s'\n", kubeConfig)
	return config
}

func loadKubernetesConfig(kubeConfig string) (*rest.Config, error) {
	if kubeConfig == "" {
		return nil, errors.New("kubeConfig not set")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("error building Kubernetes client configuration: %w", err)
	}
	// Informers and recorder Pod-proxy queries share this client. Keep recorder
	// polling from exhausting client-go's default low QPS token bucket.
	config.QPS = e2eClientQPS
	config.Burst = e2eClientBurst
	return config, nil
}

// InitializeKubernetesClient creates a client for tests that only need direct API calls.
// Unlike InitializeClient, it does not start shared informers or wait for cache sync.
func InitializeKubernetesClient(t *testing.T) *kubernetes.Clientset {
	config := buildKubernetesConfig(t)
	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("error creating Kubernetes client: %v", err)
	}
	return k8sClientSet
}

func InitializeClient(ctx context.Context, t *testing.T) (*kubernetes.Clientset, *v1alpha1.Clientset) {
	config := buildKubernetesConfig(t)
	k8sClientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("error creating Kubernetes client: %v", err)
	}
	crdClientSet, err := v1alpha1.NewForConfig(config)
	if err != nil {
		t.Fatalf("error creating CRD client: %v", err)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
	crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

	podInformer := factory.Core().V1().Pods().Informer()
	modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

	defer runtime.HandleCrash()
	factory.Start(ctx.Done())
	crdFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced, modelInformer.HasSynced) {
		t.Fatal("timed out waiting for caches to sync")
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

// WaitForInference polls the public gateway until the model is actually
// servable. Kubernetes objects may exist before HTTPRoute backend references
// and gateway caches have converged, so object existence alone is not enough.
func WaitForInference(t *testing.T, modelName string) {
	t.Helper()
	config := LoadConfig()
	client := NewOpenAIClient(config.GatewayURL, config.APIKey)
	err := wait.PollUntilContextTimeout(context.Background(), time.Second, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.UserMessage("Say this is a readiness check"),
				},
				Model: modelName,
			})
			if err != nil {
				t.Logf("waiting for inference route for %s: %v", modelName, err)
				return false, nil
			}
			return true, nil
		})
	require.NoError(t, err, "model %s did not become servable", modelName)
}

// WaitForStableAdapterInference checks both adapters in each round so that each
// remains independently servable while the other is in use.
func WaitForStableAdapterInference(t *testing.T, firstModel, secondModel string) {
	t.Helper()
	config := LoadConfig()
	client := NewOpenAIClient(config.GatewayURL, config.APIKey)
	start := time.Now()
	err := wait.PollUntilContextTimeout(context.Background(), time.Second, 30*time.Second, true,
		stableAdapterInferenceCondition(t, client, firstModel, secondModel))
	elapsed := time.Since(start)
	var probeErr *inferenceProbeError
	if errors.As(err, &probeErr) {
		// The caller's cleanup reads each log once. Poll for this probe first.
		captureEnvoyProbeLog(t, config.GatewayNamespace, probeErr.probe)
	}
	require.NoError(t, err, "waiting for adapters %s and %s after %s", firstModel, secondModel, elapsed)
	t.Logf("both adapters served three consecutive rounds in %s", time.Since(start).Round(time.Millisecond))
}

type inferenceProbeError struct {
	probe   string
	message string
}

func (e *inferenceProbeError) Error() string {
	return e.message
}

func stableAdapterInferenceCondition(
	t *testing.T, client openai.Client, firstModel, secondModel string,
) wait.ConditionWithContextFunc {
	rounds := 0
	return func(ctx context.Context) (bool, error) {
		ready := true
		for _, model := range [...]string{firstModel, secondModel} {
			// Envoy's default access log records User-Agent, even when a local
			// reply has no response request ID or Envoy replaces x-request-id.
			probe := "aibrix-e2e/" + uuid.NewString()
			err := checkInference(ctx, client, model, option.WithHeader("User-Agent", probe))
			if err == nil {
				continue
			}
			var apiErr *openai.Error
			// The default HTTPRoute readiness check marks its own 503. A plain
			// upstream 503 or a 500 must surface on this attempt.
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusServiceUnavailable &&
				apiErr.Response != nil && apiErr.Response.Header.Get("x-error-routing") == "true" &&
				apiErr.Code == "service_unavailable" {
				t.Logf("waiting for %s route (%s): %s", model, probe, inferenceFailureDetail(err))
				ready = false
				continue
			}
			return false, &inferenceProbeError{
				probe:   probe,
				message: fmt.Sprintf("inference for %s (%s): %s", model, probe, inferenceFailureDetail(err)),
			}
		}
		if !ready {
			rounds = 0
			return false, nil
		}
		rounds++
		return rounds == 3, nil
	}
}

func ValidateInferenceWithClient(t *testing.T, client openai.Client, modelName string) {
	t.Helper()
	if err := checkInference(context.Background(), client, modelName); err != nil {
		t.Fatalf("chat completion for %s: %s", modelName, inferenceFailureDetail(err))
	}
}

func checkInference(ctx context.Context, client openai.Client, modelName string, opts ...option.RequestOption) error {
	completion, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Say this is a test")},
		Model:    modelName,
	}, opts...)
	if err != nil {
		return err
	}
	if completion == nil {
		return errors.New("returned no completion")
	}
	if completion.Model != modelName {
		return fmt.Errorf("returned model %q", completion.Model)
	}
	if len(completion.Choices) == 0 {
		return errors.New("returned no choices")
	}
	return nil
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

// inferenceFailureDetail includes the raw body for non-OpenAI gateway errors.
func inferenceFailureDetail(err error) string {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return fmt.Sprintf("%v", err)
	}
	body, _ := io.ReadAll(apiErr.Response.Body)
	return fmt.Sprintf("%v (request-id: %q, x-request-id: %q, response body: %q)",
		err,
		apiErr.Response.Header.Get("request-id"),
		apiErr.Response.Header.Get("x-request-id"),
		body)
}

const envoyContainer = "envoy"

func gatewayProxySelector(namespace string) string {
	return "gateway.envoyproxy.io/owning-gateway-namespace=" + namespace +
		",gateway.envoyproxy.io/owning-gateway-name=aibrix-eg"
}

func recentPodLogOptions(container string) *corev1.PodLogOptions {
	return &corev1.PodLogOptions{
		Container:    container,
		SinceSeconds: ptr.To[int64](300),
		TailLines:    ptr.To[int64](1000),
	}
}

func findProbeAccessLog(logs []byte, probe string) (string, bool) {
	for _, line := range bytes.Split(logs, []byte("\n")) {
		var entry struct {
			UserAgent string `json:"user-agent"`
		}
		if err := json.Unmarshal(line, &entry); err == nil && entry.UserAgent == probe {
			return string(line), true
		}
	}
	return "", false
}

func waitForEnvoyProbeLog(
	ctx context.Context, client kubernetes.Interface, namespace, probe string, interval time.Duration,
) (string, string, error) {
	var matchedPod, matchedLine string
	var lastListErr, lastReadErr error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		pods, err := client.CoreV1().Pods(v1.NamespaceAll).List(ctx, v1.ListOptions{
			LabelSelector: gatewayProxySelector(namespace),
		})
		if err != nil {
			if ctx.Err() == nil {
				lastListErr = fmt.Errorf("listing Envoy pods: %w", err)
			}
			return false, nil
		}
		for _, pod := range pods.Items {
			logs, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, recentPodLogOptions(envoyContainer)).DoRaw(ctx)
			if err != nil {
				if ctx.Err() == nil {
					lastReadErr = fmt.Errorf("reading %s/%s %s logs: %w", pod.Namespace, pod.Name, envoyContainer, err)
				}
				continue
			}
			if line, found := findProbeAccessLog(logs, probe); found {
				matchedPod, matchedLine = pod.Namespace+"/"+pod.Name, line
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		err = errors.Join(err, lastListErr, lastReadErr)
	}
	return matchedPod, matchedLine, err
}

func captureEnvoyProbeLog(t *testing.T, namespace, probe string) {
	t.Helper()
	config, err := loadKubernetesConfig(os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Logf("cannot capture Envoy access log for probe %s: %v", probe, err)
		return
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Logf("cannot capture Envoy access log for probe %s: creating Kubernetes client: %v", probe, err)
		return
	}
	const timeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pod, line, err := waitForEnvoyProbeLog(ctx, client, namespace, probe, 500*time.Millisecond)
	if err != nil {
		t.Logf("Envoy access log for probe %s not found within %s: %v. "+
			"The entry may be delayed, never emitted, or outside the retained log window.", probe, timeout, err)
		return
	}
	t.Logf("Envoy access log for probe %s from pod %s:\n%s", probe, pod, line)
}

// DumpGatewayLogs collects the access log of the installation's Envoy proxy
// and the ext_proc plugin logs. Each source has its own time budget.
func DumpGatewayLogs(t *testing.T, client kubernetes.Interface) {
	t.Helper()
	namespace := LoadConfig().GatewayNamespace
	dumpPodLogs(t, client, v1.NamespaceAll, gatewayProxySelector(namespace), envoyContainer)
	dumpPodLogs(t, client, namespace, "app=gateway-plugins", "gateway-plugin")
}

func dumpPodLogs(t *testing.T, client kubernetes.Interface, namespace, selector, container string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Logf("listing pods for %s logs: %v", container, err)
		return
	}
	if len(pods.Items) == 0 {
		t.Logf("no pods matching %s for %s logs", selector, container)
	}
	for _, pod := range pods.Items {
		logs, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, recentPodLogOptions(container)).DoRaw(ctx)
		if err != nil {
			t.Logf("reading %s/%s %s logs: %v", pod.Namespace, pod.Name, container, err)
			continue
		}
		t.Logf("%s/%s %s logs:\n%s", pod.Namespace, pod.Name, container, logs)
	}
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
