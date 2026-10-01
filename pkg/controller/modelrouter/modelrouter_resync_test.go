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

package modelrouter

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

var leaderWorkerSetGVK = schema.GroupVersionKind{
	Group:   "leaderworkerset.x-k8s.io",
	Version: "v1",
	Kind:    "LeaderWorkerSet",
}

// createCountingClient counts HTTPRoute creates so tests can assert that a
// resync or update did not write to the API server.
type createCountingClient struct {
	client.Client
	routeCreates int
}

func (c *createCountingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*gatewayv1.HTTPRoute); ok {
		c.routeCreates++
	}
	return c.Client.Create(ctx, obj, opts...)
}

func httpRouteExists(t *testing.T, c client.Client, modelName string) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: aibrixEnvoyGatewayNamespace,
		Name:      utils.ModelRouterName(modelName),
	}, &gatewayv1.HTTPRoute{})
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get HTTPRoute for model %q: %v", modelName, err)
	}
	return false
}

func deleteHTTPRoute(t *testing.T, c client.Client, modelName string) {
	t.Helper()
	route := getHTTPRoute(t, c, modelName)
	if err := c.Delete(context.Background(), route); err != nil {
		t.Fatalf("delete HTTPRoute for model %q: %v", modelName, err)
	}
}

func TestModelRouteMetadataChanged(t *testing.T) {
	base := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					constants.ModelLabelName: "llama-7b",
					constants.ModelLabelPort: "8000",
					"app":                    "llama",
				},
				Annotations: map[string]string{
					constants.ModelAnnoServiceName:      "llama-svc",
					constants.ModelAnnoRouterCustomPath: "/v1/custom",
				},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*appsv1.Deployment)
		want   bool
	}{
		{
			name:   "no change",
			mutate: func(*appsv1.Deployment) {},
			want:   false,
		},
		{
			name:   "unrelated label changed",
			mutate: func(d *appsv1.Deployment) { d.Labels["app"] = "other" },
			want:   false,
		},
		{
			name:   "unrelated annotation added",
			mutate: func(d *appsv1.Deployment) { d.Annotations["note"] = "x" },
			want:   false,
		},
		{
			name:   "model name label changed",
			mutate: func(d *appsv1.Deployment) { d.Labels[constants.ModelLabelName] = "llama-13b" },
			want:   true,
		},
		{
			name:   "model name label removed",
			mutate: func(d *appsv1.Deployment) { delete(d.Labels, constants.ModelLabelName) },
			want:   true,
		},
		{
			name: "model name moved from label to annotation with same value",
			mutate: func(d *appsv1.Deployment) {
				delete(d.Labels, constants.ModelLabelName)
				d.Annotations[constants.ModelLabelName] = "llama-7b"
			},
			want: false,
		},
		{
			name:   "port changed",
			mutate: func(d *appsv1.Deployment) { d.Labels[constants.ModelLabelPort] = "8080" },
			want:   true,
		},
		{
			name:   "service name changed",
			mutate: func(d *appsv1.Deployment) { d.Annotations[constants.ModelAnnoServiceName] = "other-svc" },
			want:   true,
		},
		{
			name:   "custom paths changed",
			mutate: func(d *appsv1.Deployment) { d.Annotations[constants.ModelAnnoRouterCustomPath] = "/v2/custom" },
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldObj, newObj := base(), base()
			tt.mutate(newObj)
			if got := modelRouteMetadataChanged(oldObj, newObj); got != tt.want {
				t.Errorf("modelRouteMetadataChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateRouteFromWorkload(t *testing.T) {
	const modelName = "late-model"
	now := metav1.Now()

	unlabeledDeployment := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "late-deploy",
				Namespace: "models",
				Labels:    map[string]string{"app": "late"},
			},
		}
	}
	labeledDeployment := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "late-deploy",
				Namespace: "models",
				Labels:    modelWorkloadLabels(modelName, "8000"),
			},
		}
	}

	tests := []struct {
		name      string
		oldObj    interface{}
		newObj    interface{}
		wantRoute bool
	}{
		{
			name:      "deployment model label added later",
			oldObj:    unlabeledDeployment(),
			newObj:    labeledDeployment(),
			wantRoute: true,
		},
		{
			name: "model adapter model label added later",
			oldObj: &modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{Name: "late-adapter", Namespace: "models"},
			},
			newObj: &modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "late-adapter",
					Namespace: "models",
					Labels:    modelWorkloadLabels(modelName, "8000"),
				},
			},
			wantRoute: true,
		},
		{
			name: "unstructured model label added later",
			oldObj: func() interface{} {
				u := labeledLeaderWorkerSet("models", "late-lws", modelName)
				u.SetLabels(nil)
				return u
			}(),
			newObj:    labeledLeaderWorkerSet("models", "late-lws", modelName),
			wantRoute: true,
		},
		{
			name:   "model metadata unchanged",
			oldObj: labeledDeployment(),
			newObj: func() interface{} {
				d := labeledDeployment()
				d.Labels["app"] = "changed"
				return d
			}(),
			wantRoute: false,
		},
		{
			name:   "workload being deleted",
			oldObj: unlabeledDeployment(),
			newObj: func() interface{} {
				d := labeledDeployment()
				d.DeletionTimestamp = &now
				return d
			}(),
			wantRoute: false,
		},
		{
			name:      "model label removed",
			oldObj:    labeledDeployment(),
			newObj:    unlabeledDeployment(),
			wantRoute: false,
		},
		{
			name:      "non-object input is ignored",
			oldObj:    "not-an-object",
			newObj:    labeledDeployment(),
			wantRoute: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newEventTestRouter(t)
			m.updateRouteFromWorkload(tt.oldObj, tt.newObj)

			if got := httpRouteExists(t, m.Client, modelName); got != tt.wantRoute {
				t.Errorf("HTTPRoute exists = %v, want %v", got, tt.wantRoute)
			}
		})
	}
}

func TestUpdateRouteFromWorkloadRecreatesMissingRouteOnPortChange(t *testing.T) {
	const modelName = "llama-7b"
	oldDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llama-deploy",
			Namespace: "models",
			Labels:    modelWorkloadLabels(modelName, "8000"),
		},
	}
	m := newEventTestRouter(t)
	m.addRouteFromDeployment(oldDeploy)
	deleteHTTPRoute(t, m.Client, modelName)

	newDeploy := oldDeploy.DeepCopy()
	newDeploy.Labels[constants.ModelLabelPort] = "8080"
	m.updateRouteFromWorkload(oldDeploy, newDeploy)

	route := getHTTPRoute(t, m.Client, modelName)
	backend := route.Spec.Rules[0].BackendRefs[0]
	if backend.Port == nil || int32(*backend.Port) != 8080 {
		t.Errorf("backend port = %v, want 8080", backend.Port)
	}
}

func TestEnsureHTTPRoutesRecreatesMissingRoutes(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deepseek-deploy",
			Namespace: "models",
			Labels:    modelWorkloadLabels("deepseek-coder-7b", "8000"),
		},
	}
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lora-adapter",
			Namespace: "adapters",
			Labels:    modelWorkloadLabels("lora-model", "8000"),
		},
	}
	unlabeled := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plain-app",
			Namespace: "models",
			Labels:    map[string]string{"app": "plain"},
		},
	}

	m := newEventTestRouter(t, deploy, adapter, unlabeled)
	if err := m.ensureHTTPRoutes(context.Background()); err != nil {
		t.Fatalf("ensureHTTPRoutes() error = %v", err)
	}
	route := getHTTPRoute(t, m.Client, "deepseek-coder-7b")
	if got := route.Spec.Rules[0].BackendRefs[0].Namespace; got == nil || string(*got) != "models" {
		t.Errorf("backend namespace = %v, want models", got)
	}
	_ = getHTTPRoute(t, m.Client, "lora-model")
	_ = getReferenceGrant(t, m.Client, "models")

	// Simulate an out-of-band deletion, as reported in the issue.
	deleteHTTPRoute(t, m.Client, "deepseek-coder-7b")
	if err := m.ensureHTTPRoutes(context.Background()); err != nil {
		t.Fatalf("ensureHTTPRoutes() error = %v", err)
	}
	_ = getHTTPRoute(t, m.Client, "deepseek-coder-7b")
}

func TestEnsureHTTPRoutesDoesNotWriteWhenRoutesExist(t *testing.T) {
	deploys := []*appsv1.Deployment{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy-a",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		},
		// A second workload serving the same model must not cause a second create.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama-deploy-b",
				Namespace: "models",
				Labels:    modelWorkloadLabels("llama-7b", "8000"),
			},
		},
	}
	m := newEventTestRouter(t, deploys[0], deploys[1])
	m.addRouteFromDeployment(deploys[0])

	counter := &createCountingClient{Client: m.Client}
	m.Client = counter
	if err := m.ensureHTTPRoutes(context.Background()); err != nil {
		t.Fatalf("ensureHTTPRoutes() error = %v", err)
	}
	if counter.routeCreates != 0 {
		t.Errorf("HTTPRoute creates = %d, want 0", counter.routeCreates)
	}
}

func TestEnsureHTTPRoutesSkipsDeletingWorkloads(t *testing.T) {
	now := metav1.Now()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "llama-deploy",
			Namespace:         "models",
			Labels:            modelWorkloadLabels("llama-7b", "8000"),
			DeletionTimestamp: &now,
			// The fake client rejects objects with a deletion timestamp but no finalizer.
			Finalizers: []string{"test.aibrix.ai/hold"},
		},
	}
	m := newEventTestRouter(t, deploy)
	if err := m.ensureHTTPRoutes(context.Background()); err != nil {
		t.Fatalf("ensureHTTPRoutes() error = %v", err)
	}
	if httpRouteExists(t, m.Client, "llama-7b") {
		t.Error("HTTPRoute was created for a workload being deleted")
	}
}

func TestEnsureHTTPRoutesListsRegisteredUnstructuredWorkloads(t *testing.T) {
	m := newEventTestRouter(t)
	m.workloadGVKs = []schema.GroupVersionKind{leaderWorkerSetGVK}
	m.cacheReader = &listHookClient{
		Client: m.Client,
		hook: func(ctx context.Context, base client.Client, list client.ObjectList, opts ...client.ListOption) error {
			uList, ok := list.(*unstructured.UnstructuredList)
			if !ok || !isLeaderWorkerSetList(uList) {
				return fmt.Errorf("unexpected list %T from cacheReader", list)
			}
			uList.Items = []unstructured.Unstructured{*labeledLeaderWorkerSet("lws-ns", "llama-lws", "llama-lws-model")}
			return nil
		},
	}

	if err := m.ensureHTTPRoutes(context.Background()); err != nil {
		t.Fatalf("ensureHTTPRoutes() error = %v", err)
	}
	_ = getHTTPRoute(t, m.Client, "llama-lws-model")
}

func TestEnsureHTTPRoutesReturnsListError(t *testing.T) {
	m := newEventTestRouter(t)
	m.Client = &listHookClient{
		Client: m.Client,
		hook: func(ctx context.Context, base client.Client, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("api unavailable")
		},
	}
	if err := m.ensureHTTPRoutes(context.Background()); err == nil {
		t.Fatal("ensureHTTPRoutes() omitted list error")
	}
}

func TestRunResyncsUntilContextCancelled(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llama-deploy",
			Namespace: "models",
			Labels:    modelWorkloadLabels("llama-7b", "8000"),
		},
	}
	m := newEventTestRouter(t, deploy)
	m.resyncInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !httpRouteExists(t, m.Client, "llama-7b") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run did not recreate the missing HTTPRoute")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
