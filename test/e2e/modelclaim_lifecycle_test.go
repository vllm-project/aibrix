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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	lifecycleNamespace      = "default"
	lifecycleDeploymentName = "modelclaim-lifecycle-pool"
	lifecyclePoolName       = "modelclaim-lifecycle"
	lifecycleBusyClaim      = "lifecycle-busy"
	lifecycleBusyModel      = "lifecycle-busy-model"
	lifecycleIdleClaim      = "lifecycle-idle"
	lifecycleIdleModel      = "lifecycle-idle-model"
	lifecycleRuntimePort    = 8080
	lifecycleBusyPort       = 20000
	lifecycleIdlePort       = 20001
)

type lifecycleRuntimeSnapshot struct {
	Models []struct {
		ModelName              string `json:"model_name"`
		Port                   int32  `json:"port"`
		Phase                  string `json:"phase"`
		Alive                  bool   `json:"alive"`
		Ready                  bool   `json:"ready"`
		RequestMetricsObserved bool   `json:"request_metrics_observed"`
	} `json:"models"`
}

type lifecycleRouteBinding struct {
	Model string `json:"model"`
	Port  int32  `json:"port"`
	State string `json:"state"`
}

func TestModelClaimLifecycleAndRequestWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k8sClient, aibrixClient := initializeClient(ctx, t)

	cleanupLifecycleResources(t, k8sClient, aibrixClient)
	deployment := lifecyclePoolDeployment()
	_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Create(
		ctx, deployment, metav1.CreateOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupLifecycleResources(t, k8sClient, aibrixClient)
	})

	pod := waitForLifecyclePoolPod(t, ctx, k8sClient)

	createLifecycleClaim(t, ctx, aibrixClient, lifecycleBusyClaim, lifecycleBusyModel)
	busy := waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleBusyClaim, modelv1alpha1.ModelClaimActive,
	)
	require.Len(t, busy.Status.Instances, 1)
	assert.Equal(t, int32(lifecycleBusyPort), busy.Status.Instances[0].Port)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 30*time.Second)

	createLifecycleClaim(t, ctx, aibrixClient, lifecycleIdleClaim, lifecycleIdleModel)
	idle := waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleIdleClaim, modelv1alpha1.ModelClaimActive,
	)
	require.Len(t, idle.Status.Instances, 1)
	assert.Equal(t, int32(lifecycleIdlePort), idle.Status.Instances[0].Port)

	idle = waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleIdleClaim, modelv1alpha1.ModelClaimSleeping,
	)
	assert.Equal(t, int32(0), idle.Status.ReadyReplicas)
	assertLifecycleRouteBinding(
		t, ctx, k8sClient, pod.Name, lifecycleIdleClaim,
		lifecycleRouteBinding{
			Model: lifecycleIdleModel,
			Port:  0,
			State: constants.ModelClaimRoutingStateSleeping,
		},
	)

	snapshot := getLifecycleRuntimeSnapshot(t, ctx, k8sClient, pod.Name)
	assertLifecycleSnapshotModel(t, snapshot, lifecycleIdleModel, lifecycleIdlePort, "sleeping", false, false)
	assertLifecycleSnapshotModel(t, snapshot, lifecycleBusyModel, lifecycleBusyPort, "active", true, true)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 10*time.Second)

	assertLifecycleWakeResponse(t, lifecycleIdleModel, 10*time.Second)

	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 10*time.Second)
	waitForLifecycleClaimPhase(
		t, ctx, aibrixClient, lifecycleIdleClaim, modelv1alpha1.ModelClaimActive,
	)
	waitForLifecycleModelStatus(t, lifecycleIdleModel, http.StatusOK, 30*time.Second)
	waitForLifecycleModelStatus(t, lifecycleBusyModel, http.StatusOK, 10*time.Second)
}

func lifecyclePoolDeployment() *appsv1.Deployment {
	labels := map[string]string{
		"app":                           lifecycleDeploymentName,
		constants.ModelPoolLabelName:    lifecyclePoolName,
		constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue,
		constants.ModelLabelEngine:      "vllm",
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lifecycleDeploymentName,
			Namespace: lifecycleNamespace,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{"lifecycle":{"sleepAfterSeconds":5}}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app": lifecycleDeploymentName,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To[int64](1),
					Containers: []corev1.Container{
						lifecycleRuntimeContainer(),
						lifecycleEngineContainer(
							"busy-engine", lifecycleBusyModel, lifecycleBusyPort,
							`{"success_total":1,"running":1,"waiting":0,"swapped":0}`,
						),
						lifecycleEngineContainer(
							"idle-engine", lifecycleIdleModel, lifecycleIdlePort,
							`{"success_total":0,"running":0,"waiting":0,"swapped":0}`,
						),
					},
				},
			},
		},
	}
}

func lifecycleRuntimeContainer() corev1.Container {
	return corev1.Container{
		Name:            "aibrix-runtime",
		Image:           "aibrix/runtime:nightly",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--port", fmt.Sprint(lifecycleRuntimePort)},
		Env: []corev1.EnvVar{
			{Name: "AIBRIX_MODEL_RUNTIME_MOCK", Value: "1"},
			{Name: "AIBRIX_MODEL_RUNTIME_MOCK_EXTERNAL_ENGINES", Value: "1"},
			{Name: "AIBRIX_WEIGHT_CACHE_DIR", Value: "/tmp/aibrix-model-cache"},
			{Name: "ENABLE_KVCACHED", Value: "false"},
			{Name: "KVCACHED_AUTOPATCH", Value: "0"},
		},
		Ports: []corev1.ContainerPort{{Name: "runtime", ContainerPort: lifecycleRuntimePort}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.FromInt32(lifecycleRuntimePort),
			}},
			PeriodSeconds:    1,
			FailureThreshold: 30,
		},
	}
}

func lifecycleEngineContainer(name, model string, port int32, metrics string) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           "aibrix/vllm-mock:nightly",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "MODEL_NAME", Value: model},
			{Name: "DEPLOYMENT_NAME", Value: lifecycleDeploymentName},
			{Name: "SERVER_PORT", Value: fmt.Sprint(port)},
			{Name: "METRICS_OVERRIDES", Value: metrics},
			{Name: "STANDALONE_MODE", Value: "true"},
			{Name: "SIMULATION", Value: "disabled"},
			{Name: "MOCK_REQUEST_DURATION_SECONDS", Value: "0"},
		},
		Ports: []corev1.ContainerPort{{Name: name, ContainerPort: port}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(port),
			}},
			PeriodSeconds:    1,
			FailureThreshold: 30,
		},
	}
}

func createLifecycleClaim(
	t *testing.T,
	ctx context.Context,
	client *modelclient.Clientset,
	name string,
	model string,
) {
	t.Helper()
	_, err := client.ModelV1alpha1().ModelClaims(lifecycleNamespace).Create(
		ctx,
		&modelv1alpha1.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lifecycleNamespace},
			Spec: modelv1alpha1.ModelClaimSpec{
				ModelName: ptr.To(model),
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					constants.ModelPoolLabelName: lifecyclePoolName,
				}},
				ArtifactURL: "huggingface://aibrix/" + model,
				Engine:      "vllm",
			},
		},
		metav1.CreateOptions{},
	)
	require.NoError(t, err)
}

func waitForLifecyclePoolPod(
	t *testing.T,
	ctx context.Context,
	client *kubernetes.Clientset,
) *corev1.Pod {
	t.Helper()
	var readyPod *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true,
		func(ctx context.Context) (bool, error) {
			pods, err := client.CoreV1().Pods(lifecycleNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + lifecycleDeploymentName,
			})
			if err != nil || len(pods.Items) != 1 {
				return false, err
			}
			pod := &pods.Items[0]
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					readyPod = pod.DeepCopy()
					return true, nil
				}
			}
			return false, nil
		})
	require.NoError(t, err, "ModelClaim lifecycle pool pod did not become ready")
	return readyPod
}

func waitForLifecycleClaimPhase(
	t *testing.T,
	ctx context.Context,
	client *modelclient.Clientset,
	name string,
	expected modelv1alpha1.ModelClaimPhase,
) *modelv1alpha1.ModelClaim {
	t.Helper()
	var latest *modelv1alpha1.ModelClaim
	err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true,
		func(ctx context.Context) (bool, error) {
			claim, err := client.ModelV1alpha1().ModelClaims(lifecycleNamespace).Get(
				ctx, name, metav1.GetOptions{},
			)
			if err != nil {
				return false, err
			}
			latest = claim
			if claim.Status.Phase == modelv1alpha1.ModelClaimFailed && expected != claim.Status.Phase {
				return false, fmt.Errorf("ModelClaim %s entered Failed: %+v", name, claim.Status.Conditions)
			}
			return claim.Status.Phase == expected, nil
		})
	require.NoError(t, err, "ModelClaim %s did not reach %s; latest=%+v", name, expected, latest)
	return latest
}

func assertLifecycleRouteBinding(
	t *testing.T,
	ctx context.Context,
	client *kubernetes.Clientset,
	podName string,
	claimName string,
	expected lifecycleRouteBinding,
) {
	t.Helper()
	pod, err := client.CoreV1().Pods(lifecycleNamespace).Get(ctx, podName, metav1.GetOptions{})
	require.NoError(t, err)
	raw := pod.Annotations[constants.ModelClaimPodAnnotationPrefix+claimName]
	require.NotEmpty(t, raw)
	var binding lifecycleRouteBinding
	require.NoError(t, json.Unmarshal([]byte(raw), &binding))
	assert.Equal(t, expected, binding)
}

func getLifecycleRuntimeSnapshot(
	t *testing.T,
	ctx context.Context,
	client *kubernetes.Clientset,
	podName string,
) lifecycleRuntimeSnapshot {
	t.Helper()
	raw, err := client.CoreV1().Pods(lifecycleNamespace).ProxyGet(
		"http", podName, fmt.Sprint(lifecycleRuntimePort), "v1/runtime/snapshot", nil,
	).DoRaw(ctx)
	require.NoError(t, err)
	var snapshot lifecycleRuntimeSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	return snapshot
}

func assertLifecycleSnapshotModel(
	t *testing.T,
	snapshot lifecycleRuntimeSnapshot,
	model string,
	port int32,
	phase string,
	ready bool,
	metricsObserved bool,
) {
	t.Helper()
	for _, observed := range snapshot.Models {
		if observed.ModelName != model {
			continue
		}
		assert.Equal(t, port, observed.Port)
		assert.Equal(t, phase, observed.Phase)
		assert.True(t, observed.Alive)
		assert.Equal(t, ready, observed.Ready)
		assert.Equal(t, metricsObserved, observed.RequestMetricsObserved)
		return
	}
	t.Fatalf("runtime snapshot does not contain model %s: %+v", model, snapshot.Models)
}

func sendLifecycleModelRequest(model string) (*http.Response, []byte, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"model": model,
		"messages": []map[string]string{{
			"role": "user", "content": "modelclaim lifecycle check",
		}},
	})
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequest(
		http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader(payload),
	)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("routing-strategy", "random")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, nil, err
	}
	body, readErr := io.ReadAll(response.Body)
	return response, body, readErr
}

func waitForLifecycleModelStatus(t *testing.T, model string, expected int, timeout time.Duration) {
	t.Helper()
	var lastStatus int
	var lastBody string
	err := wait.PollUntilContextTimeout(context.Background(), time.Second, timeout, true,
		func(context.Context) (bool, error) {
			response, body, err := sendLifecycleModelRequest(model)
			if err != nil {
				lastBody = err.Error()
				return false, nil
			}
			lastStatus = response.StatusCode
			lastBody = string(body)
			_ = response.Body.Close()
			return lastStatus == expected, nil
		})
	require.NoError(
		t, err, "model %s did not return %d; last status=%d body=%s",
		model, expected, lastStatus, lastBody,
	)
}

func assertLifecycleWakeResponse(t *testing.T, model string, timeout time.Duration) {
	t.Helper()
	var lastStatus int
	var lastBody string
	var retryAfter string
	err := wait.PollUntilContextTimeout(context.Background(), time.Second, timeout, true,
		func(context.Context) (bool, error) {
			response, body, err := sendLifecycleModelRequest(model)
			if err != nil {
				lastBody = err.Error()
				return false, nil
			}
			lastStatus = response.StatusCode
			lastBody = string(body)
			retryAfter = response.Header.Get("Retry-After")
			_ = response.Body.Close()
			return lastStatus == http.StatusServiceUnavailable, nil
		})
	require.NoError(
		t, err, "model %s did not return 503; last status=%d body=%s",
		model, lastStatus, lastBody,
	)
	assert.Equal(t, "10", retryAfter)
}

func cleanupLifecycleResources(
	t *testing.T,
	k8sClient *kubernetes.Clientset,
	aibrixClient *modelclient.Clientset,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	claims := aibrixClient.ModelV1alpha1().ModelClaims(lifecycleNamespace)
	for _, name := range []string{lifecycleIdleClaim, lifecycleBusyClaim} {
		err := claims.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("delete ModelClaim %s: %v", name, err)
		}
	}
	_ = wait.PollUntilContextTimeout(ctx, time.Second, 20*time.Second, true,
		func(ctx context.Context) (bool, error) {
			for _, name := range []string{lifecycleIdleClaim, lifecycleBusyClaim} {
				_, err := claims.Get(ctx, name, metav1.GetOptions{})
				if err == nil {
					return false, nil
				}
				if !apierrors.IsNotFound(err) {
					return false, err
				}
			}
			return true, nil
		})
	err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Delete(
		ctx, lifecycleDeploymentName, metav1.DeleteOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("delete ModelClaim lifecycle Deployment: %v", err)
	}
	_ = wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := k8sClient.AppsV1().Deployments(lifecycleNamespace).Get(
				ctx, lifecycleDeploymentName, metav1.GetOptions{},
			)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	err = waitForLifecyclePoolPodsDeleted(ctx, k8sClient, time.Second)
	require.NoError(t, err, "timed out waiting for ModelClaim lifecycle pool pods to be deleted")
}

func waitForLifecyclePoolPodsDeleted(
	ctx context.Context,
	client kubernetes.Interface,
	interval time.Duration,
) error {
	return wait.PollUntilContextCancel(ctx, interval, true,
		func(ctx context.Context) (bool, error) {
			pods, err := client.CoreV1().Pods(lifecycleNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + lifecycleDeploymentName,
			})
			if err != nil {
				return false, err
			}
			return len(pods.Items) == 0, nil
		})
}
