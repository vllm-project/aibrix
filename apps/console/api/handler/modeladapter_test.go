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

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	aibrixclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	aibrixfake "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

const modelAdapterTestNamespace = "model-serving"

type staticModelAdapterClientProvider struct {
	kubeClient  kubernetes.Interface
	modelClient aibrixclient.Interface
	namespace   string
}

type modelAdapterLifecycleFixture struct {
	deployment  *appsv1.Deployment
	pod         *corev1.Pod
	kubeClient  kubernetes.Interface
	modelClient aibrixclient.Interface
	mux         *runtime.ServeMux
}

func (p staticModelAdapterClientProvider) Client() (kubernetes.Interface, string, error) {
	return p.kubeClient, p.namespace, nil
}

func (p staticModelAdapterClientProvider) ModelClient() (aibrixclient.Interface, string, error) {
	return p.modelClient, p.namespace, nil
}

func TestModelAdapterHandlerLifecycle(t *testing.T) {
	fixture := newModelAdapterLifecycleFixture(t)
	fixture.assertTargets(t)
	created := fixture.createAdapter(t)
	fixture.assertAdapterStatus(t, created)
	fixture.deleteAdapter(t, created)
}

func newModelAdapterLifecycleFixture(t *testing.T) *modelAdapterLifecycleFixture {
	t.Helper()
	now := metav1.NewTime(time.Now().UTC().Add(-time.Minute))
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "qwen-serving",
			Namespace:         modelAdapterTestNamespace,
			CreationTimestamp: now,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "qwen-serving"},
			},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                    "qwen-serving",
						constants.ModelLabelName: "Qwen/Qwen2.5-7B",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "engine",
						Image: "example/vllm:latest",
						Env: []corev1.EnvVar{{
							Name:  engineTypeEnvName,
							Value: "vllm",
						}},
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: 8000,
						}},
					}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "qwen-serving-abc",
			Namespace:         modelAdapterTestNamespace,
			CreationTimestamp: now,
			Labels:            map[string]string{"app": "qwen-serving"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "kind-worker",
			Containers: []corev1.Container{{Name: "engine"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.244.1.10",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "engine",
				Ready:        true,
				RestartCount: 1,
				Image:        "example/vllm:latest",
				ImageID:      "example/vllm@sha256:test",
			}},
		},
	}

	kubeClient := kubernetesfake.NewSimpleClientset(deployment, pod)
	modelClient := aibrixfake.NewSimpleClientset()
	handler := NewModelAdapterHandler(staticModelAdapterClientProvider{
		kubeClient:  kubeClient,
		modelClient: modelClient,
		namespace:   modelAdapterTestNamespace,
	})
	mux := runtime.NewServeMux()
	if err := handler.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() error = %v", err)
	}

	return &modelAdapterLifecycleFixture{
		deployment:  deployment,
		pod:         pod,
		kubeClient:  kubeClient,
		modelClient: modelClient,
		mux:         mux,
	}
}

func (f *modelAdapterLifecycleFixture) assertTargets(t *testing.T) {
	t.Helper()
	targetsRecorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelAdapterTargetAPIPath, nil)
	if targetsRecorder.Code != http.StatusOK {
		t.Fatalf("list targets status = %d, body = %s", targetsRecorder.Code, targetsRecorder.Body.String())
	}
	var targets listModelAdapterTargetsResponse
	if err := json.Unmarshal(targetsRecorder.Body.Bytes(), &targets); err != nil {
		t.Fatalf("decode targets: %v", err)
	}
	if len(targets.Targets) != 1 || targets.Targets[0].Name != f.deployment.Name {
		t.Fatalf("targets = %#v", targets.Targets)
	}
	if targets.Targets[0].BaseModel != "Qwen/Qwen2.5-7B" || targets.Targets[0].ReadyReplicas != 2 {
		t.Fatalf("target details = %#v", targets.Targets[0])
	}
}

func (f *modelAdapterLifecycleFixture) createAdapter(t *testing.T) *modelv1alpha1.ModelAdapter {
	t.Helper()
	createBody := []byte(`{
		"name":"sql-assistant",
		"artifact_url":"huggingface://example/sql-assistant",
		"deployment_name":"qwen-serving",
		"placement":"all"
	}`)
	createRecorder := serveModelAdapterRequest(t, f.mux, http.MethodPost, modelAdapterAPIPath, createBody)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRecorder.Code, createRecorder.Body.String())
	}

	created, err := f.modelClient.ModelV1alpha1().ModelAdapters(modelAdapterTestNamespace).Get(
		context.Background(),
		"sql-assistant",
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get created ModelAdapter: %v", err)
	}
	if created.Spec.Replicas != nil {
		t.Fatalf("replicas = %v, want nil for all-pods placement", *created.Spec.Replicas)
	}
	if created.Spec.BaseModel == nil || *created.Spec.BaseModel != f.deployment.Name {
		t.Fatalf("base model = %v", created.Spec.BaseModel)
	}
	if created.Spec.PodSelector.MatchLabels["app"] != "qwen-serving" {
		t.Fatalf("pod selector = %#v", created.Spec.PodSelector)
	}
	if created.Labels[constants.ModelLabelName] != created.Name ||
		created.Annotations[targetDeploymentAnnotation] != f.deployment.Name {
		t.Fatalf("metadata = labels %#v, annotations %#v", created.Labels, created.Annotations)
	}
	return created
}

func (f *modelAdapterLifecycleFixture) assertAdapterStatus(
	t *testing.T,
	created *modelv1alpha1.ModelAdapter,
) {
	t.Helper()
	created.Status = modelv1alpha1.ModelAdapterStatus{
		Phase:           modelv1alpha1.ModelAdapterRunning,
		Candidates:      2,
		ReadyReplicas:   1,
		DesiredReplicas: 2,
		Instances:       []string{f.pod.Name},
	}
	if _, err := f.modelClient.ModelV1alpha1().ModelAdapters(modelAdapterTestNamespace).UpdateStatus(
		context.Background(),
		created,
		metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	getRecorder := serveModelAdapterRequest(
		t,
		f.mux,
		http.MethodGet,
		modelAdapterAPIPath+"/sql-assistant",
		nil,
	)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRecorder.Code, getRecorder.Body.String())
	}
	var detail modelAdapterResponse
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Phase != "Running" || detail.Target == nil || detail.Target.Name != f.deployment.Name {
		t.Fatalf("detail = %#v", detail)
	}
	if detail.BaseModel != "Qwen/Qwen2.5-7B" {
		t.Fatalf("display base model = %q", detail.BaseModel)
	}
	if len(detail.Instances) != 1 || detail.Instances[0].Ready != "1/1" ||
		detail.Instances[0].PodIP != f.pod.Status.PodIP {
		t.Fatalf("instances = %#v", detail.Instances)
	}

	listRecorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelAdapterAPIPath, nil)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRecorder.Code, listRecorder.Body.String())
	}
	var list listModelAdaptersResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.ModelAdapters) != 1 || list.ModelAdapters[0].Name != created.Name {
		t.Fatalf("list = %#v", list.ModelAdapters)
	}
}

func (f *modelAdapterLifecycleFixture) deleteAdapter(
	t *testing.T,
	created *modelv1alpha1.ModelAdapter,
) {
	t.Helper()
	deleteRecorder := serveModelAdapterRequest(
		t,
		f.mux,
		http.MethodDelete,
		modelAdapterAPIPath+"/sql-assistant",
		nil,
	)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if _, err := f.kubeClient.AppsV1().Deployments(modelAdapterTestNamespace).Get(
		context.Background(),
		f.deployment.Name,
		metav1.GetOptions{},
	); err != nil {
		t.Fatalf("base deployment was changed by adapter delete: %v", err)
	}
	if _, err := f.modelClient.ModelV1alpha1().ModelAdapters(modelAdapterTestNamespace).Get(
		context.Background(),
		created.Name,
		metav1.GetOptions{},
	); err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("get deleted ModelAdapter error = %v, want NotFound", err)
	}
}

func TestModelAdapterHandlerRequiresAdminForMutations(t *testing.T) {
	handler := NewModelAdapterHandler(staticModelAdapterClientProvider{
		kubeClient:  kubernetesfake.NewSimpleClientset(),
		modelClient: aibrixfake.NewSimpleClientset(),
		namespace:   modelAdapterTestNamespace,
	})
	mux := runtime.NewServeMux()
	if err := handler.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() error = %v", err)
	}

	body := []byte(`{
		"name":"sql-assistant",
		"artifact_url":"huggingface://example/sql-assistant",
		"deployment_name":"qwen-serving",
		"placement":"all"
	}`)
	viewerCreate := serveModelAdapterRequestAsRole(
		t, mux, http.MethodPost, modelAdapterAPIPath, body, "viewer",
	)
	if viewerCreate.Code != http.StatusForbidden {
		t.Fatalf("viewer create status = %d, want %d", viewerCreate.Code, http.StatusForbidden)
	}
	viewerDelete := serveModelAdapterRequestAsRole(
		t, mux, http.MethodDelete, modelAdapterAPIPath+"/sql-assistant", nil, "viewer",
	)
	if viewerDelete.Code != http.StatusForbidden {
		t.Fatalf("viewer delete status = %d, want %d", viewerDelete.Code, http.StatusForbidden)
	}
	unauthenticated := serveModelAdapterRequestAsRole(
		t, mux, http.MethodDelete, modelAdapterAPIPath+"/sql-assistant", nil, "",
	)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}
}

func TestIsModelAdapterTarget(t *testing.T) {
	base := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "model"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					constants.ModelLabelName:   "base-model",
					constants.ModelLabelEngine: modelAdapterVLLMEngine,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: defaultModelServingPort,
					}},
				}}},
			},
		},
	}

	if !isModelAdapterTarget(base) {
		t.Fatal("expected explicitly configured vLLM Deployment to be compatible")
	}

	unrelated := base.DeepCopy()
	unrelated.Spec.Template.Labels = map[string]string{}
	if isModelAdapterTarget(unrelated) {
		t.Fatal("unrelated Deployment was accepted as a ModelAdapter target")
	}

	unsupportedEngine := base.DeepCopy()
	unsupportedEngine.Spec.Template.Labels[constants.ModelLabelEngine] = "trtllm"
	if isModelAdapterTarget(unsupportedEngine) {
		t.Fatal("unsupported engine was accepted as a ModelAdapter target")
	}

	unsupportedPort := base.DeepCopy()
	unsupportedPort.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort = 9000
	if isModelAdapterTarget(unsupportedPort) {
		t.Fatal("non-8000 engine port was accepted as a ModelAdapter target")
	}
}

func TestModelAdapterHandlerRejectsUnsupportedArtifactScheme(t *testing.T) {
	kubeClient := kubernetesfake.NewSimpleClientset()
	modelClient := aibrixfake.NewSimpleClientset()
	handler := NewModelAdapterHandler(staticModelAdapterClientProvider{
		kubeClient:  kubeClient,
		modelClient: modelClient,
		namespace:   modelAdapterTestNamespace,
	})
	mux := runtime.NewServeMux()
	if err := handler.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() error = %v", err)
	}

	body := []byte(`{
		"name":"sql-assistant",
		"artifact_url":"hdfs://model-store/lora/sql-assistant",
		"deployment_name":"qwen-serving",
		"placement":"single"
	}`)
	recorder := serveModelAdapterRequest(t, mux, http.MethodPost, modelAdapterAPIPath, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func serveModelAdapterRequest(
	t *testing.T,
	mux http.Handler,
	method string,
	path string,
	body []byte,
) *httptest.ResponseRecorder {
	return serveModelAdapterRequestAsRole(t, mux, method, path, body, modelAdapterAdminRole)
}

func serveModelAdapterRequestAsRole(
	t *testing.T,
	mux http.Handler,
	method string,
	path string,
	body []byte,
	role string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if role != "" {
		user := &middleware.UserInfo{ID: "test-user", Role: role}
		request = request.WithContext(context.WithValue(request.Context(), middleware.UserContextKey, user))
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}
