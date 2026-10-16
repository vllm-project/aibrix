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

package discovery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	modelclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	gatewayName      = "aibrix-gateway-plugins"
	repositoryRoot   = "../../.."
	controlNamespace = "aibrix-system"
	foreignNamespace = "other-installation"
)

type installation struct {
	name           string
	namespace      string
	helmValues     []string
	splitKustomize bool
}

var installations = []installation{
	{name: "kustomize", namespace: controlNamespace},
	{name: "kustomize-split", namespace: "workloads", splitKustomize: true},
	{name: "helm", namespace: controlNamespace, helmValues: []string{}},
	{name: "helm-split", namespace: "workloads", helmValues: []string{
		"gatewayPlugin.installationNamespace=workloads", "gatewayPlugin.serviceAccount.name=custom-gateway",
	}},
	{name: "helm-sidecar", namespace: "workloads", helmValues: []string{
		"gatewayPlugin.installationNamespace=workloads", "gateway.enable=false", "gateway.envoyAsSideCar=true",
	}},
}

func renderInstallation(t *testing.T, install installation) []map[string]interface{} {
	t.Helper()
	if install.helmValues != nil {
		args := []string{"template", "aibrix", filepath.Join(repositoryRoot, "dist/chart"), "--namespace", controlNamespace}
		for _, value := range install.helmValues {
			args = append(args, "--set", value)
		}
		return renderManifest(t, "helm", args...)
	}
	base := filepath.Join(repositoryRoot, "config/default")
	if !install.splitKustomize {
		return renderManifest(t, "kubectl", "kustomize", base)
	}
	// Apply the documented split-namespace configuration to the real default base.
	base, err := filepath.Abs(base)
	require.NoError(t, err)
	// macOS temporary directories may be reached through /var -> /private/var.
	// Resolve both paths before computing the relative Kustomize resource path.
	base, err = filepath.EvalSymlinks(base)
	require.NoError(t, err)
	overlay, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	base, err = filepath.Rel(overlay, base)
	require.NoError(t, err)
	config := `resources:
- ` + base + `
patches:
- target:
    kind: Deployment
    name: aibrix-gateway-plugins
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/args
      value: ["--installation-namespace=workloads"]
- target:
    kind: Role
    name: aibrix-gateway-plugins-role
  patch: |-
    - op: replace
      path: /metadata/namespace
      value: workloads
- target:
    kind: RoleBinding
    name: aibrix-gateway-plugins-rolebinding
  patch: |-
    - op: replace
      path: /metadata/namespace
      value: workloads
    - op: replace
      path: /subjects
      value:
      - kind: ServiceAccount
        name: aibrix-gateway-plugins
        namespace: aibrix-system
- target:
    kind: ClusterRole
    name: aibrix-gateway-namespace-reader
  patch: |-
    - op: replace
      path: /rules/0/resourceNames
      value: [workloads]
`
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(config), 0600))
	return renderManifest(t, "kubectl", "kustomize", overlay, "--load-restrictor", "LoadRestrictionsNone")
}

func renderManifest(t *testing.T, command string, args ...string) []map[string]interface{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	require.NoError(t, err, "%s", stderr.String())
	return decodeManifest(t, data)
}

func decodeManifest(t *testing.T, data []byte) []map[string]interface{} {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []map[string]interface{}
	for {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if object != nil {
			objects = append(objects, object)
		}
	}
	return objects
}

func objectJSON(t *testing.T, object map[string]interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return data
}

// Exercise the rendered manifests with a real API server and RBAC authorizer.
// KUBEBUILDER_ASSETS, kubectl and helm must be available.
func TestRenderedGatewayDiscoveryPermissions(t *testing.T) {
	for _, install := range installations {
		t.Run(install.name, func(t *testing.T) {
			verifyPermissions(t, install, renderInstallation(t, install))
		})
	}
}

func verifyPermissions(t *testing.T, install installation, objects []map[string]interface{}) {
	t.Helper()
	namespace := install.namespace
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repositoryRoot, "config/crd/model")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	admin, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	namespaces := map[string]bool{namespace: true, controlNamespace: true, foreignNamespace: true}
	for name := range namespaces {
		_, err := admin.CoreV1().Namespaces().Create(ctx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	applied := 0
	var serviceAccount string
	for _, object := range objects {
		metadata := object["metadata"].(map[string]interface{})
		name := metadata["name"].(string)
		if name == gatewayName && object["kind"] == "Deployment" {
			var deployment appsv1.Deployment
			require.NoError(t, json.Unmarshal(objectJSON(t, object), &deployment))
			serviceAccount = deployment.Spec.Template.Spec.ServiceAccountName
			expectedArg := "--installation-namespace=" + namespace
			if install.helmValues == nil && !install.splitKustomize {
				expectedArg = "--installation-namespace=$(POD_NAMESPACE)"
			}
			found := false
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name == "gateway-plugin" {
					require.Contains(t, container.Args, expectedArg)
					found = true
				}
			}
			require.True(t, found)
		}
		if object["apiVersion"] != rbacv1.SchemeGroupVersion.String() || !strings.HasPrefix(name, "aibrix-gateway-") {
			continue
		}
		resource, err := runtime.Decode(scheme.Codecs.UniversalDeserializer(), objectJSON(t, object))
		require.NoError(t, err)
		switch resource := resource.(type) {
		case *rbacv1.Role:
			_, err = admin.RbacV1().Roles(resource.Namespace).Create(ctx, resource, metav1.CreateOptions{})
		case *rbacv1.RoleBinding:
			_, err = admin.RbacV1().RoleBindings(resource.Namespace).Create(ctx, resource, metav1.CreateOptions{})
		case *rbacv1.ClusterRole:
			_, err = admin.RbacV1().ClusterRoles().Create(ctx, resource, metav1.CreateOptions{})
		case *rbacv1.ClusterRoleBinding:
			_, err = admin.RbacV1().ClusterRoleBindings().Create(ctx, resource, metav1.CreateOptions{})
		default:
			t.Fatalf("unexpected RBAC resource %T", resource)
		}
		require.NoError(t, err)
		applied++
	}
	require.Equal(t, 6, applied, "discovery, HTTPRoute and namespace reader grant pairs")
	require.NotEmpty(t, serviceAccount)
	for _, ns := range []string{namespace, foreignNamespace} {
		_, err := admin.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "same-model", Namespace: ns, Labels: map[string]string{"model.aibrix.ai/name": "same-model"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "example.invalid/model:test"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	gatewayConfig := rest.CopyConfig(config)
	gatewayConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + controlNamespace + ":" + serviceAccount,
	}
	gateway, err := kubernetes.NewForConfig(gatewayConfig)
	require.NoError(t, err)
	adapters, err := modelclient.NewForConfig(gatewayConfig)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := gateway.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		return err == nil
	}, 10*time.Second, 20*time.Millisecond)
	namespaces[""] = true
	for ns := range namespaces {
		_, podListErr := gateway.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		podWatch, podWatchErr := gateway.CoreV1().Pods(ns).Watch(ctx, metav1.ListOptions{})
		if podWatch != nil {
			podWatch.Stop()
		}
		_, adapterListErr := adapters.ModelV1alpha1().ModelAdapters(ns).List(ctx, metav1.ListOptions{})
		adapterWatch, adapterWatchErr := adapters.ModelV1alpha1().ModelAdapters(ns).Watch(ctx, metav1.ListOptions{})
		if adapterWatch != nil {
			adapterWatch.Stop()
		}
		for _, err := range []error{podListErr, podWatchErr, adapterListErr, adapterWatchErr} {
			if ns == namespace {
				require.NoError(t, err)
			} else {
				require.True(t, apierrors.IsForbidden(err), "%q: %v", ns, err)
			}
		}
	}
	_, err = gateway.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = gateway.CoreV1().Namespaces().Get(ctx, foreignNamespace, metav1.GetOptions{})
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	_, err = gateway.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	for _, check := range []struct {
		namespace, group, resource, verb string
		allowed                          bool
	}{
		{controlNamespace, "gateway.networking.k8s.io", "httproutes", "get", true},
		{foreignNamespace, "gateway.networking.k8s.io", "httproutes", "get", false},
		{controlNamespace, "gateway.networking.k8s.io", "httproutes", "create", false},
		{namespace, "", "pods", "create", false},
		{namespace, "model.aibrix.ai", "modeladapters", "update", false},
	} {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: check.namespace, Group: check.group, Resource: check.resource,
				Verb: check.verb, Name: "same-model-router",
			}},
		}
		result, err := gateway.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Equal(t, check.allowed, result.Status.Allowed, "%+v: %s", check, result.Status.Reason)
	}
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	var pods []*corev1.Pod
	err = discovery.NewKubernetesProvider(gatewayConfig, namespace).Watch(func(event discovery.WatchEvent) {
		if pod, ok := event.Object.(*corev1.Pod); ok {
			mu.Lock()
			defer mu.Unlock()
			pods = append(pods, pod)
		}
	}, stop)
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, pods, 1)
	require.Equal(t, namespace, pods[0].Namespace)
	mu.Unlock()
	foreignProvider := discovery.NewKubernetesProvider(gatewayConfig, foreignNamespace)
	require.Error(t, foreignProvider.Watch(func(discovery.WatchEvent) {}, stop))
}
