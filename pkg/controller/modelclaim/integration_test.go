//go:build integration

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

package modelclaim

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// TestModelClaimControllerIntegration runs the REAL ModelClaim controller
// against a REAL kube-apiserver (controller-runtime envtest, no GPU). It proves
// the end-to-end control plane works: CRD install + validation, finalizer,
// candidate discovery, runtime activation (faked), status subresource, and the
// routing-annotation patch that makes the model routable.
//
// Run with: go test -tags integration ./pkg/controller/modelclaim/... -run Integration
// (requires envtest binaries: `make envtest && bin/setup-envtest use 1.29.0 --bin-dir bin`).
func TestModelClaimControllerIntegration(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.29.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	defer func() { _ = testEnv.Stop() }()

	require.NoError(t, modelv1alpha1.AddToScheme(scheme.Scheme))
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	ctx := context.Background()
	const ns = "default"

	// A real warm GPU pod, Running with an IP.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-1",
			Namespace: ns,
			Labels: map[string]string{
				constants.ModelPoolLabelName:    "pool-a",
				constants.ModelPoolLabelEnabled: constants.ModelPoolLabelEnabledValue,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "aibrix-runtime"}}},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.1"
	require.NoError(t, k8sClient.Status().Update(ctx, pod))

	// A real ModelClaim selecting the pool.
	pm := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen2-7b", Namespace: ns},
		Spec: modelv1alpha1.ModelClaimSpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: "pool-a"}},
			ArtifactURL: "huggingface://Qwen/Qwen2-7B-Instruct",
			Engine:      "vllm",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pm))

	// The real reconciler against the real apiserver; only the GPU-touching runtime
	// is faked (no GPU available in CI).
	r := &ModelClaimReconciler{
		Client:   k8sClient,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(32),
		Runtime:  &fakeRuntime{},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "qwen2-7b"}}
	// First pass installs the finalizer; subsequent passes place + activate.
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
	}

	// The model is Active with one recorded instance.
	got := &modelv1alpha1.ModelClaim{}
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, got))
	assert.Equal(t, modelv1alpha1.ModelClaimActive, got.Status.Phase)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.NotZero(t, got.Status.Instances[0].Port)

	// The warm pod carries the routing annotation the gateway cache reads.
	gotPod := &corev1.Pod{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "warm-1"}, gotPod))
	val := gotPod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"]
	assert.Contains(t, val, `"model":"qwen2-7b"`)
	assert.Contains(t, val, fmt.Sprintf(`"port":%d`, got.Status.Instances[0].Port))

	// Deleting the ModelClaim deactivates and removes the routing annotation.
	require.NoError(t, k8sClient.Delete(ctx, got))
	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
	}
	afterPod := &corev1.Pod{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "warm-1"}, afterPod))
	_, stillThere := afterPod.Annotations[constants.ModelClaimPodAnnotationPrefix+"qwen2-7b"]
	assert.False(t, stillThere, "routing annotation should be removed after deletion")

}
