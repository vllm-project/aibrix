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
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	aibrixclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	aibrixfake "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const modelClaimTestPool = "b300-pool-a"

type modelClaimFixture struct {
	pod         *corev1.Pod
	kubeClient  kubernetes.Interface
	modelClient aibrixclient.Interface
	mux         *runtime.ServeMux
}

func newModelClaimFixture(t *testing.T, objects ...*modelv1alpha1.ModelClaim) *modelClaimFixture {
	t.Helper()
	now := metav1.NewTime(time.Now().UTC().Add(-time.Minute))
	poolLabels := map[string]string{
		"app":                           "warm-runtime-pool-b300",
		constants.ModelPoolLabelName:    modelClaimTestPool,
		constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue,
	}
	gpuResources := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{utils.NVIDIAGPUResourceName: resource.MustParse("1")},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-runtime-pool-b300",
			Namespace:         modelAdapterTestNamespace,
			CreationTimestamp: now,
			Annotations: map[string]string{
				constants.ModelPoolPolicyAnnotationKey: `{"lifecycle":{"sleepAfterSeconds":60}}`,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "warm-runtime-pool-b300"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: poolLabels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:      "aibrix-runtime",
					Image:     "aibrix/kvcached-runtime:dev",
					Resources: gpuResources,
				}}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-runtime-pool-b300-abc",
			Namespace:         modelAdapterTestNamespace,
			CreationTimestamp: now,
			Labels:            poolLabels,
		},
		Spec: corev1.PodSpec{
			NodeName: "gpu-node-1",
			Containers: []corev1.Container{{
				Name:      "aibrix-runtime",
				Image:     "aibrix/kvcached-runtime:dev",
				Resources: gpuResources,
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.244.1.20",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "aibrix-runtime",
				Ready: true,
			}},
		},
	}
	// A Pod that carries the pool name but not the enabled label is counted,
	// and is not a placement candidate.
	drained := pod.DeepCopy()
	drained.Name = "warm-runtime-pool-b300-drained"
	drained.Labels = map[string]string{constants.ModelPoolLabelName: modelClaimTestPool}

	claims := make([]k8sruntime.Object, 0, len(objects))
	for _, claim := range objects {
		claims = append(claims, claim)
	}
	kubeClient := kubernetesfake.NewSimpleClientset(deployment, pod, drained)
	modelClient := aibrixfake.NewSimpleClientset(claims...)
	handler := NewModelClaimHandler(staticModelAdapterClientProvider{
		kubeClient:  kubeClient,
		modelClient: modelClient,
		namespace:   modelAdapterTestNamespace,
	})
	mux := runtime.NewServeMux()
	if err := handler.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes() error = %v", err)
	}
	return &modelClaimFixture{pod: pod, kubeClient: kubeClient, modelClient: modelClient, mux: mux}
}

const createModelClaimBody = `{
	"name":"qwen3-0-6b",
	"model_name":"qwen3-0.6b",
	"artifact_url":"huggingface://Qwen/Qwen3-0.6B",
	"pool_name":"b300-pool-a",
	"per_gpu":{"maximum_footprint":"6Gi","kv_floor":"1Gi"},
	"priority":10,
	"engine_args":{" --max-model-len ":" 2048 ","--enable-prefix-caching":""}
}`

var createdModelClaimArgs = map[string]string{"--max-model-len": "2048", "--enable-prefix-caching": ""}

func TestModelClaimHandlerLifecycle(t *testing.T) {
	f := newModelClaimFixture(t)
	f.assertPools(t, 0)
	created := f.createClaim(t)
	f.placeClaim(t, created)
	f.assertDetail(t)
	f.assertList(t)
	f.assertPools(t, 1)
	f.updatePerGPU(t, created)
	f.deleteClaim(t)
}

func (f *modelClaimFixture) assertPools(t *testing.T, wantClaims int32) {
	t.Helper()
	pools := f.listPools(t)
	if len(pools) != 1 {
		t.Fatalf("pools = %#v", pools)
	}
	pool := pools[0]
	if pool.Name != modelClaimTestPool || pool.Pods != 2 || pool.EnabledPods != 1 || pool.ReadyPods != 1 {
		t.Fatalf("pool counts = %#v", pool)
	}
	if len(pool.GPUCounts) != 1 || pool.GPUCounts[0] != 1 {
		t.Fatalf("pool GPU counts = %v", pool.GPUCounts)
	}
	if len(pool.Workloads) != 1 || pool.Workloads[0] != "warm-runtime-pool-b300" ||
		pool.Policy == "" || len(pool.Images) != 1 || pool.Claims != wantClaims {
		t.Fatalf("pool details = %#v", pool)
	}
}

func (f *modelClaimFixture) createClaim(t *testing.T) *modelv1alpha1.ModelClaim {
	t.Helper()
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodPost, modelClaimAPIPath, []byte(createModelClaimBody))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	created := f.getClaim(t, "qwen3-0-6b")
	if created.Spec.ModelName == nil || *created.Spec.ModelName != "qwen3-0.6b" {
		t.Fatalf("model name = %v", created.Spec.ModelName)
	}
	if created.Spec.PodSelector.MatchLabels[constants.ModelPoolLabelName] != modelClaimTestPool ||
		len(created.Spec.PodSelector.MatchLabels) != 1 {
		t.Fatalf("pod selector = %#v", created.Spec.PodSelector)
	}
	if created.Spec.Engine != modelClaimVLLMEngine || created.Spec.Replicas != nil {
		t.Fatalf("engine = %q, replicas = %v", created.Spec.Engine, created.Spec.Replicas)
	}
	if created.Spec.PerGPU == nil ||
		created.Spec.PerGPU.MaximumFootprint.Cmp(resource.MustParse("6Gi")) != 0 ||
		created.Spec.PerGPU.KVFloor.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("perGPU = %#v", created.Spec.PerGPU)
	}
	assertModelClaimArgs(t, created.Spec.EngineConfig)
	if created.Spec.Priority != 10 {
		t.Fatalf("priority = %d", created.Spec.Priority)
	}
	if created.Labels[modelAdapterManagedByLabel] != modelAdapterManagedByLabelValue {
		t.Fatalf("labels = %#v", created.Labels)
	}
	return created
}

func assertModelClaimArgs(t *testing.T, config *modelv1alpha1.ModelClaimEngineConfig) {
	t.Helper()
	if config == nil || len(config.Args) != len(createdModelClaimArgs) {
		t.Fatalf("engine config = %#v", config)
	}
	for key, value := range createdModelClaimArgs {
		if got, found := config.Args[key]; !found || got != value {
			t.Fatalf("engine arg %s = %q (found %v), want %q", key, got, found, value)
		}
	}
}

func (f *modelClaimFixture) assertDetail(t *testing.T) {
	t.Helper()
	detail := f.getDetail(t, "qwen3-0-6b")
	if detail.ModelName != "qwen3-0.6b" || detail.PoolName != modelClaimTestPool || detail.Phase != "Sleeping" ||
		!detail.ManagedByConsole || detail.PerGPU == nil || detail.PerGPU.MaximumFootprint != "6Gi" ||
		detail.EngineArgs["--max-model-len"] != "2048" || detail.Priority != 10 {
		t.Fatalf("detail = %#v", detail)
	}
	wantConditions := []string{"Initialized", "Scheduled", "Ready"}
	if len(detail.Conditions) != len(wantConditions) {
		t.Fatalf("conditions = %#v", detail.Conditions)
	}
	for i, conditionType := range wantConditions {
		if detail.Conditions[i].Type != conditionType {
			t.Fatalf("condition %d = %q, want %q", i, detail.Conditions[i].Type, conditionType)
		}
	}
	if len(detail.Instances) != 1 {
		t.Fatalf("instances = %#v", detail.Instances)
	}
	instance := detail.Instances[0]
	if instance.Pod != f.pod.Name || instance.Port != 20000 || instance.KVLimitBytes != 1<<30 ||
		instance.Reason != "WaitingForRoom" || instance.WakeRequestedAt != "2026-10-01T08:00:00Z" {
		t.Fatalf("instance = %#v", instance)
	}
	if instance.Route == nil || instance.Route.State != constants.ModelClaimRoutingStateSleeping ||
		instance.Route.Port != 0 || !instance.Route.WakeByRequest || instance.Route.Reason != "WaitingForRoom" {
		t.Fatalf("route = %#v", instance.Route)
	}
	if instance.PodStatus.Node != "gpu-node-1" || instance.PodStatus.PodIP != f.pod.Status.PodIP {
		t.Fatalf("pod status = %#v", instance.PodStatus)
	}
}

func (f *modelClaimFixture) assertList(t *testing.T) {
	t.Helper()
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelClaimAPIPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var list listModelClaimsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.ModelClaims) != 1 || len(list.ModelClaims[0].Instances) != 1 ||
		list.ModelClaims[0].Instances[0].PodStatus.Ready != "1/1" {
		t.Fatalf("list = %#v", list.ModelClaims)
	}
}

func (f *modelClaimFixture) updatePerGPU(t *testing.T, created *modelv1alpha1.ModelClaim) {
	t.Helper()
	recorder := serveModelAdapterRequest(
		t, f.mux, http.MethodPatch, modelClaimAPIPath+"/qwen3-0-6b",
		[]byte(`{"per_gpu":{"maximum_footprint":"8Gi","kv_floor":"2Gi"}}`),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	updated := f.getClaim(t, "qwen3-0-6b")
	if updated.Spec.PerGPU.MaximumFootprint.Cmp(resource.MustParse("8Gi")) != 0 ||
		updated.Spec.PerGPU.KVFloor.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("updated perGPU = %#v", updated.Spec.PerGPU)
	}
	if updated.Spec.ArtifactURL != created.Spec.ArtifactURL {
		t.Fatalf("update changed the artifact: %#v", updated.Spec)
	}
	assertModelClaimArgs(t, updated.Spec.EngineConfig)
}

func (f *modelClaimFixture) deleteClaim(t *testing.T) {
	t.Helper()
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodDelete, modelClaimAPIPath+"/qwen3-0-6b", nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, err := f.modelClient.ModelV1alpha1().ModelClaims(modelAdapterTestNamespace).Get(
		context.Background(), "qwen3-0-6b", metav1.GetOptions{},
	); !apierrors.IsNotFound(err) {
		t.Fatalf("get deleted ModelClaim error = %v, want NotFound", err)
	}
	if _, err := f.kubeClient.CoreV1().Pods(modelAdapterTestNamespace).Get(
		context.Background(), f.pod.Name, metav1.GetOptions{},
	); err != nil {
		t.Fatalf("warm pool Pod was changed by claim delete: %v", err)
	}
}

func TestModelClaimHandlerShowsClaimsAppliedOutsideTheConsole(t *testing.T) {
	external := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: modelAdapterTestNamespace},
		Spec: modelv1alpha1.ModelClaimSpec{
			PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      constants.ModelPoolLabelName,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"a", "b"},
			}}},
			ArtifactURL: "s3://models/llama",
		},
	}
	f := newModelClaimFixture(t, external)

	detail := f.getDetail(t, "llama")
	if detail.ModelName != "llama" || detail.Engine != modelClaimVLLMEngine || detail.Phase != "Pending" ||
		detail.PoolName != "" || detail.PodSelector == "" || detail.PerGPU != nil || detail.ManagedByConsole {
		t.Fatalf("detail = %#v", detail)
	}
	if detail.EngineArgs == nil || detail.Conditions == nil || detail.Instances == nil {
		t.Fatalf("collections must encode as empty arrays and objects: %#v", detail)
	}
}

func modelClaimPlacedOn(name, pod string) *modelv1alpha1.ModelClaim {
	claim := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: modelAdapterTestNamespace},
		Spec:       modelv1alpha1.ModelClaimSpec{ArtifactURL: "huggingface://Qwen/" + name},
	}
	if pod != "" {
		claim.Status = modelv1alpha1.ModelClaimStatus{
			Phase: modelv1alpha1.ModelClaimActive,
			Instances: []modelv1alpha1.ModelClaimInstance{{
				Pod: pod, Port: 20000, Phase: modelv1alpha1.ModelClaimActive,
			}},
		}
	}
	return claim
}

// podActions returns the Pod lists (by label selector) and gets (by name) the
// handler sent.
func podActions(client *kubernetesfake.Clientset) (lists, gets []string) {
	for _, action := range client.Actions() {
		if action.GetResource().Resource != "pods" {
			continue
		}
		switch typed := action.(type) {
		case k8stesting.ListAction:
			lists = append(lists, typed.GetListRestrictions().Labels.String())
		case k8stesting.GetAction:
			gets = append(gets, typed.GetName())
		}
	}
	sort.Strings(gets)
	return lists, gets
}

func TestModelClaimHandlerListReadsOnlyInstancePods(t *testing.T) {
	f := newModelClaimFixture(t,
		modelClaimPlacedOn("pooled", "warm-runtime-pool-b300-abc"),
		// A claim whose selector does not use the pool label.
		modelClaimPlacedOn("selected", "custom-runtime"),
		// An instance kept on a Pod that has left its pool.
		modelClaimPlacedOn("departed", "departed-runtime"),
		modelClaimPlacedOn("gone", "deleted-runtime"),
	)
	selected := f.pod.DeepCopy()
	selected.Name = "custom-runtime"
	selected.Labels = map[string]string{constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue}
	departed := f.pod.DeepCopy()
	departed.Name = "departed-runtime"
	departed.Labels = map[string]string{"app": "retired"}
	for _, pod := range []*corev1.Pod{selected, departed} {
		if _, err := f.kubeClient.CoreV1().Pods(modelAdapterTestNamespace).Create(
			context.Background(), pod, metav1.CreateOptions{},
		); err != nil {
			t.Fatalf("create Pod %s: %v", pod.Name, err)
		}
	}
	fake := f.kubeClient.(*kubernetesfake.Clientset)
	fake.ClearActions()

	recorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelClaimAPIPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var list listModelClaimsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	wantStatus := map[string]string{"pooled": "Running", "selected": "Running", "departed": "Running", "gone": "Unknown"}
	for _, claim := range list.ModelClaims {
		if len(claim.Instances) != 1 || claim.Instances[0].PodStatus.Status != wantStatus[claim.Name] {
			t.Fatalf("claim %s instances = %#v, want Pod status %q", claim.Name, claim.Instances, wantStatus[claim.Name])
		}
	}
	if len(list.ModelClaims) != len(wantStatus) {
		t.Fatalf("list = %#v", list.ModelClaims)
	}

	lists, gets := podActions(fake)
	if len(lists) != 1 || lists[0] != constants.ModelPoolLabelEnabled {
		t.Fatalf("Pod lists = %q, want one by %q", lists, constants.ModelPoolLabelEnabled)
	}
	if want := []string{"deleted-runtime", "departed-runtime"}; strings.Join(gets, ",") != strings.Join(want, ",") {
		t.Fatalf("Pod gets = %q, want %q", gets, want)
	}
}

func TestModelClaimHandlerListReadsNoPodsWithoutInstances(t *testing.T) {
	for _, claims := range [][]*modelv1alpha1.ModelClaim{nil, {modelClaimPlacedOn("pending", "")}} {
		f := newModelClaimFixture(t, claims...)
		fake := f.kubeClient.(*kubernetesfake.Clientset)
		fake.ClearActions()

		recorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelClaimAPIPath, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("list status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if lists, gets := podActions(fake); len(lists) != 0 || len(gets) != 0 {
			t.Fatalf("%d claims: Pod lists = %q, gets = %q, want none", len(claims), lists, gets)
		}
	}
}

func TestModelClaimHandlerRejectsInvalidCreateRequests(t *testing.T) {
	existing := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: modelAdapterTestNamespace},
		Spec: modelv1alpha1.ModelClaimSpec{
			ModelName:   ptr.To("qwen2.5-0.5b"),
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: modelClaimTestPool}},
			ArtifactURL: "huggingface://Qwen/Qwen2.5-0.5B-Instruct",
		},
	}
	cases := []struct {
		name       string
		edit       func(map[string]interface{})
		wantStatus int
		wantError  string
	}{
		{"name is not a DNS name", withField("name", "Qwen3"), http.StatusBadRequest, "DNS subdomain"},
		{"name does not fit an annotation key", withField("name", strings.Repeat("a", 64)), http.StatusBadRequest, "annotation key"},
		{"model name with whitespace", withField("model_name", "qwen 3"), http.StatusBadRequest, "whitespace"},
		{"unsupported artifact scheme", withField("artifact_url", "hdfs://models/qwen"), http.StatusBadRequest, "unsupported scheme"},
		{"unsupported engine", withField("engine", "trtllm"), http.StatusBadRequest, "engine must be"},
		{"missing pool", withField("pool_name", ""), http.StatusBadRequest, "pool_name is required"},
		{"unknown pool", withField("pool_name", "h100-pool"), http.StatusBadRequest, "was not found"},
		{"negative priority", withField("priority", -1), http.StatusBadRequest, "priority must not be negative"},
		{"missing perGPU", withoutField("per_gpu"), http.StatusBadRequest, "never placed"},
		{"missing KV floor", withField("per_gpu", map[string]interface{}{"maximum_footprint": "6Gi"}), http.StatusBadRequest, "kvFloor is required"},
		{"perGPU that is not a quantity", withField("per_gpu", map[string]interface{}{"maximum_footprint": "6 GB", "kv_floor": "1Gi"}), http.StatusBadRequest, "must be a quantity"},
		{"perGPU that is not positive", withField("per_gpu", map[string]interface{}{"maximum_footprint": "0", "kv_floor": "1Gi"}), http.StatusBadRequest, "not positive"},
		{"engine arg that is not a flag", withEngineArg("max-model-len", "2048"), http.StatusBadRequest, "must be an engine flag"},
		{"engine arg the launcher owns", withEngineArg("--port", "9000"), http.StatusBadRequest, "runtime sets --port"},
		{"launcher flag with its value after equals", withEngineArg("--port=9000", ""), http.StatusBadRequest, "runtime sets --port"},
		{"launcher flag with underscores", withEngineArg("--served_model_name", "x"), http.StatusBadRequest, "runtime sets --served-model-name"},
		{"launcher flag turned off", withEngineArg("--no-enable-sleep-mode", ""), http.StatusBadRequest, "runtime sets --enable-sleep-mode"},
		{"value after equals and in the value", withEngineArg("--max-num-seqs=8", "16"), http.StatusBadRequest, "leave its value empty"},
		{"one flag in two spellings", withEngineArg("--max_model_len", "4096"), http.StatusBadRequest, "sets --max-model-len more than once"},
		{"gpu memory utilization", withEngineArg("--gpu-memory-utilization", "0.9"), http.StatusBadRequest, "incompatible with kvcached"},
		{"data parallelism", withEngineArg("--data-parallel-size", "2"), http.StatusBadRequest, "unsupported by fixed topology"},
		{"topology that does not match the pool", withEngineArg("--tensor-parallel-size", "2"), http.StatusBadRequest, "have 1 GPU"},
		{"topology with its value after equals", withEngineArg("--tensor-parallel-size=2", ""), http.StatusBadRequest, "have 1 GPU"},
		{"gpu memory utilization with underscores", withEngineArg("--gpu_memory_utilization", "0.9"), http.StatusBadRequest, "incompatible with kvcached"},
		{"served name already in use", withField("model_name", "qwen2.5-0.5b"), http.StatusConflict, "is already served by ModelClaim"},
		{"unknown field", withField("replicas", 2), http.StatusBadRequest, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newModelClaimFixture(t, existing.DeepCopy())
			body := map[string]interface{}{}
			if err := json.Unmarshal([]byte(createModelClaimBody), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			tc.edit(body)
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode body: %v", err)
			}
			recorder := serveModelAdapterRequest(t, f.mux, http.MethodPost, modelClaimAPIPath, encoded)
			if recorder.Code != tc.wantStatus || !strings.Contains(recorder.Body.String(), tc.wantError) {
				t.Fatalf("status = %d, body = %s; want %d containing %q",
					recorder.Code, recorder.Body.String(), tc.wantStatus, tc.wantError)
			}
			claims, err := f.modelClient.ModelV1alpha1().ModelClaims(modelAdapterTestNamespace).List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil || len(claims.Items) != 1 {
				t.Fatalf("a refused request created a claim: %v, %#v", err, claims)
			}
		})
	}
}

func TestModelClaimHandlerAcceptsSGLangWithoutTopologyCheck(t *testing.T) {
	f := newModelClaimFixture(t)
	body := strings.Replace(createModelClaimBody, `"pool_name"`, `"engine":"sglang","pool_name"`, 1)
	body = strings.Replace(body, `"--enable-prefix-caching":""`, `"--tensor-parallel-size":"2"`, 1)
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodPost, modelClaimAPIPath, []byte(body))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if claim := f.getClaim(t, "qwen3-0-6b"); claim.Spec.Engine != modelClaimSGLangEngine {
		t.Fatalf("engine = %q", claim.Spec.Engine)
	}
}

func TestModelClaimHandlerRejectsInvalidUpdates(t *testing.T) {
	f := newModelClaimFixture(t)
	if recorder := serveModelAdapterRequest(
		t, f.mux, http.MethodPost, modelClaimAPIPath, []byte(createModelClaimBody),
	); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	cases := map[string]string{
		"missing perGPU":         `{}`,
		"immutable field":        `{"artifact_url":"huggingface://Qwen/Qwen3-8B"}`,
		"perGPU that is refused": `{"per_gpu":{"maximum_footprint":"30m","kv_floor":"1Gi"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := serveModelAdapterRequest(t, f.mux, http.MethodPatch, modelClaimAPIPath+"/qwen3-0-6b", []byte(body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if claim := f.getClaim(t, "qwen3-0-6b"); claim.Spec.PerGPU.MaximumFootprint.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Fatalf("a refused update changed perGPU: %#v", claim.Spec.PerGPU)
	}

	missing := serveModelAdapterRequest(
		t, f.mux, http.MethodPatch, modelClaimAPIPath+"/missing",
		[]byte(`{"per_gpu":{"maximum_footprint":"8Gi","kv_floor":"2Gi"}}`),
	)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing claim status = %d, body = %s", missing.Code, missing.Body.String())
	}
}

func TestModelClaimHandlerRequiresAdminForMutations(t *testing.T) {
	f := newModelClaimFixture(t)
	requests := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodPost, modelClaimAPIPath, []byte(createModelClaimBody)},
		{http.MethodPatch, modelClaimAPIPath + "/qwen3-0-6b", []byte(`{"per_gpu":{"maximum_footprint":"8Gi","kv_floor":"2Gi"}}`)},
		{http.MethodDelete, modelClaimAPIPath + "/qwen3-0-6b", nil},
	}
	for _, request := range requests {
		viewer := serveModelAdapterRequestAsRole(t, f.mux, request.method, request.path, request.body, "viewer")
		if viewer.Code != http.StatusForbidden {
			t.Fatalf("viewer %s status = %d, want %d", request.method, viewer.Code, http.StatusForbidden)
		}
		anonymous := serveModelAdapterRequestAsRole(t, f.mux, request.method, request.path, request.body, "")
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status = %d, want %d", request.method, anonymous.Code, http.StatusUnauthorized)
		}
	}
	viewerList := serveModelAdapterRequestAsRole(t, f.mux, http.MethodGet, modelClaimAPIPath, nil, "viewer")
	if viewerList.Code != http.StatusOK {
		t.Fatalf("viewer list status = %d, want %d", viewerList.Code, http.StatusOK)
	}
}

func (f *modelClaimFixture) listPools(t *testing.T) []modelClaimPoolResponse {
	t.Helper()
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelClaimPoolAPIPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list pools status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response listModelClaimPoolsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	return response.Pools
}

func (f *modelClaimFixture) getClaim(t *testing.T, name string) *modelv1alpha1.ModelClaim {
	t.Helper()
	claim, err := f.modelClient.ModelV1alpha1().ModelClaims(modelAdapterTestNamespace).Get(
		context.Background(), name, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get ModelClaim %s: %v", name, err)
	}
	return claim
}

func (f *modelClaimFixture) getDetail(t *testing.T, name string) modelClaimResponse {
	t.Helper()
	recorder := serveModelAdapterRequest(t, f.mux, http.MethodGet, modelClaimAPIPath+"/"+name, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var detail modelClaimResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return detail
}

// placeClaim records the claim as placed on the warm Pod and asleep, with the
// routing annotation and pending wake request the controller and gateway write.
func (f *modelClaimFixture) placeClaim(t *testing.T, claim *modelv1alpha1.ModelClaim) {
	t.Helper()
	now := metav1.NewTime(time.Now().UTC())
	claim.Status = modelv1alpha1.ModelClaimStatus{
		Phase:           modelv1alpha1.ModelClaimSleeping,
		Candidates:      1,
		DesiredReplicas: 1,
		Instances: []modelv1alpha1.ModelClaimInstance{{
			Pod:          f.pod.Name,
			Port:         20000,
			Phase:        modelv1alpha1.ModelClaimSleeping,
			KVLimitBytes: 1 << 30,
			Reason:       "WaitingForRoom",
		}},
		// Stored out of lifecycle order, as the controller may set them.
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "EngineSleeping", LastTransitionTime: now},
			{Type: "Initialized", Status: metav1.ConditionTrue, Reason: "ModelClaimInitialized", LastTransitionTime: now},
			{Type: "Scheduled", Status: metav1.ConditionTrue, Reason: "Placed", LastTransitionTime: now},
		},
	}
	if _, err := f.modelClient.ModelV1alpha1().ModelClaims(modelAdapterTestNamespace).UpdateStatus(
		context.Background(), claim, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	pod := f.pod.DeepCopy()
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + claim.Name:  `{"model":"qwen3-0.6b","port":0,"state":"sleeping","wakeByRequest":true,"reason":"WaitingForRoom"}`,
		constants.ModelClaimWakeAnnotationPrefix + claim.Name: "2026-10-01T08:00:00Z",
	}
	if _, err := f.kubeClient.CoreV1().Pods(modelAdapterTestNamespace).Update(
		context.Background(), pod, metav1.UpdateOptions{},
	); err != nil {
		t.Fatalf("update Pod annotations: %v", err)
	}
}

func withField(key string, value interface{}) func(map[string]interface{}) {
	return func(body map[string]interface{}) { body[key] = value }
}

func withoutField(key string) func(map[string]interface{}) {
	return func(body map[string]interface{}) { delete(body, key) }
}

func withEngineArg(key, value string) func(map[string]interface{}) {
	return func(body map[string]interface{}) {
		body["engine_args"].(map[string]interface{})[key] = value
	}
}
