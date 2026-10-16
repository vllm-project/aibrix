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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayManifestWiring(t *testing.T) {
	overlays := []string{"default", "overlays/release", "overlays/dev/gateway-plugin", "overlays/vke/default"}
	for _, overlay := range overlays {
		t.Run(overlay, func(t *testing.T) {
			objects := renderManifest(t, "kubectl", "kustomize", filepath.Join(repositoryRoot, "config", overlay))
			checkGatewayWiring(t, objects, "--installation-namespace=$(POD_NAMESPACE)", false)
		})
	}
	for _, install := range installations {
		if install.helmValues == nil {
			continue
		}
		t.Run(install.name, func(t *testing.T) {
			objects := renderInstallation(t, install)
			checkGatewayWiring(t, objects, "--installation-namespace="+install.namespace, install.name == "helm-sidecar")
		})
	}
}

func checkGatewayWiring(t *testing.T, objects []map[string]interface{}, argument string, sidecar bool) {
	t.Helper()
	deploymentFound, routeFound := false, false
	for _, object := range objects {
		data := objectJSON(t, object)
		switch object["kind"] {
		case "Deployment":
			var deployment appsv1.Deployment
			require.NoError(t, json.Unmarshal(data, &deployment))
			if deployment.Name != gatewayName {
				continue
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name == "gateway-plugin" {
					deploymentFound = true
					require.Contains(t, container.Args, argument)
				}
			}
		case "HTTPRoute":
			var route gatewayv1.HTTPRoute
			require.NoError(t, json.Unmarshal(data, &route))
			if route.Name != "aibrix-reserved-router-metadata-endpoint" {
				continue
			}
			require.False(t, sidecar)
			routeFound = true
			require.Len(t, route.Spec.Rules, 2)
			modelRule := route.Spec.Rules[0]
			require.Len(t, modelRule.Matches, 1)
			require.Equal(t, "/v1/models", *modelRule.Matches[0].Path.Value)
			require.Equal(t, gatewayv1.PathMatchPathPrefix, *modelRule.Matches[0].Path.Type)
			require.Equal(t, gatewayv1.ObjectName(gatewayName), modelRule.BackendRefs[0].Name)
			require.Equal(t, gatewayv1.PortNumber(8080), *modelRule.BackendRefs[0].Port)
			metadataRule := route.Spec.Rules[1]
			require.Len(t, metadataRule.Matches, 2)
			require.Equal(t, "/v1/files", *metadataRule.Matches[0].Path.Value)
			require.Equal(t, "/v1/batches", *metadataRule.Matches[1].Path.Value)
			require.Equal(t, gatewayv1.ObjectName("aibrix-metadata-service"), metadataRule.BackendRefs[0].Name)
			require.Equal(t, gatewayv1.PortNumber(8090), *metadataRule.BackendRefs[0].Port)
		case "ConfigMap":
			var config corev1.ConfigMap
			require.NoError(t, json.Unmarshal(data, &config))
			if config.Name != "aibrix-envoy-config" {
				continue
			}
			require.True(t, sidecar)
			routeFound = true
			checkSidecarModelsRoute(t, config.Data["envoy.yaml"])
		}
	}
	require.True(t, deploymentFound, "gateway deployment missing")
	require.True(t, routeFound, "models route missing")
}

func checkSidecarModelsRoute(t *testing.T, config string) {
	t.Helper()
	data, err := yaml.ToJSON([]byte(config))
	require.NoError(t, err)
	var bootstrap map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &bootstrap))
	listener := firstConfigObject(t, bootstrap, "static_resources", "listeners")
	chain := firstConfigObject(t, listener, "filter_chains")
	filter := firstConfigObject(t, chain, "filters")
	host := firstConfigObject(t, filter, "typed_config", "route_config", "virtual_hosts")
	route := firstConfigObject(t, host, "routes")
	match, found, err := unstructured.NestedMap(route, "match")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, map[string]interface{}{"path_separated_prefix": "/v1/models"}, match,
		"model listing must not require inference headers")
	clusterName, found, err := unstructured.NestedString(route, "route", "cluster")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "gateway_http", clusterName)
	extproc, found, err := unstructured.NestedMap(route, "typed_per_filter_config", "envoy.filters.http.ext_proc")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, map[string]interface{}{
		"@type":    "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute",
		"disabled": true,
	}, extproc)
	clusters, found, err := unstructured.NestedSlice(bootstrap, "static_resources", "clusters")
	require.NoError(t, err)
	require.True(t, found)
	upstreamFound := false
	for _, value := range clusters {
		cluster := value.(map[string]interface{})
		if cluster["name"] != clusterName {
			continue
		}
		upstreamFound = true
		endpoint := firstConfigObject(t, cluster, "load_assignment", "endpoints")
		lbEndpoint := firstConfigObject(t, endpoint, "lb_endpoints")
		socket, found, err := unstructured.NestedMap(lbEndpoint, "endpoint", "address", "socket_address")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "127.0.0.1", socket["address"])
		require.EqualValues(t, 8080, socket["port_value"])
	}
	require.True(t, upstreamFound, "gateway HTTP upstream missing")
}

func firstConfigObject(t *testing.T, object map[string]interface{}, fields ...string) map[string]interface{} {
	t.Helper()
	values, found, err := unstructured.NestedSlice(object, fields...)
	require.NoError(t, err)
	require.True(t, found, "%v", fields)
	require.NotEmpty(t, values, "%v", fields)
	result, ok := values[0].(map[string]interface{})
	require.True(t, ok, "%v", fields)
	return result
}

func TestHelmRejectsInvalidInstallationNamespace(t *testing.T) {
	for _, namespace := range []string{"ALL", "a/b", " ", "-invalid"} {
		t.Run(namespace, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "helm", "template", "aibrix", filepath.Join(repositoryRoot, "dist/chart"),
				"--namespace", controlNamespace, "--set-string", "gatewayPlugin.installationNamespace="+namespace).CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), "installationNamespace")
		})
	}
}

func TestHelmPreservesNamespaceStrings(t *testing.T) {
	for _, namespace := range []string{"123", "true", "false", "null", "on"} {
		t.Run(namespace, func(t *testing.T) {
			objects := renderManifest(t, "helm", "template", "aibrix", filepath.Join(repositoryRoot, "dist/chart"),
				"--namespace", controlNamespace, "--set-string", "gatewayPlugin.installationNamespace="+namespace)
			grants := 0
			for _, object := range objects {
				metadata := object["metadata"].(map[string]interface{})
				if metadata["name"] != gatewayName {
					continue
				}
				if object["kind"] == "Role" || object["kind"] == "RoleBinding" {
					grants++
					require.Equal(t, namespace, metadata["namespace"])
				}
			}
			require.Equal(t, 2, grants)
			checkGatewayWiring(t, objects, "--installation-namespace="+namespace, false)
		})
	}
}

func TestQuickstartWorkloadNamespaces(t *testing.T) {
	for _, sample := range []string{
		"model.yaml", "pd-model.yaml", "vke/model.yaml", "vke/pd-model.yaml",
		"tensorrt/tensor-rt.yaml", "tensorrt/tensor-rt-pd.yaml",
	} {
		t.Run(sample, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repositoryRoot, "samples/quickstart", sample))
			require.NoError(t, err)
			objects := decodeManifest(t, data)
			require.NotEmpty(t, objects)
			for _, object := range objects {
				metadata := object["metadata"].(map[string]interface{})
				require.Equal(t, controlNamespace, metadata["namespace"], "%s %s", object["kind"], metadata["name"])
			}
		})
	}
}
