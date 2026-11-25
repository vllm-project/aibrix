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

package installation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// TestKustomizeGatewayReferences uses the same plain overlay as issue #3521.
// It also applies the rendered JSON patches to check the filter map keys, which
// ordinary Kubernetes name-reference transformation cannot update.
func TestKustomizeGatewayReferences(t *testing.T) {
	for _, tt := range []struct{ name, namespace, prefix, suffix string }{
		{"default", "aibrix-system", "aibrix-", ""},
		{"namespace", "custom-gateway", "aibrix-", ""},
		{"prefix", "aibrix-system", "custom-", ""},
		{"namespace and prefix", "custom-gateway", "custom-", ""},
		{"suffix", "custom-gateway", "custom-", "-test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, err := filepath.Abs("../../../config/gateway")
			require.NoError(t, err)
			dir := t.TempDir()
			dir, err = filepath.EvalSymlinks(dir)
			require.NoError(t, err)
			base, err = filepath.EvalSymlinks(base)
			require.NoError(t, err)
			base, err = filepath.Rel(dir, base)
			require.NoError(t, err)
			overlay := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
				"namespace: %q\nnamePrefix: %q\nnameSuffix: %q\nresources:\n- %q\n",
				tt.namespace, tt.prefix, tt.suffix, base)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(overlay), 0600))
			objects := renderKustomize(t, dir)
			gateway := objectOfKind(t, objects, "Gateway")
			require.Equal(t, tt.namespace, gateway.GetNamespace())
			require.Equal(t, tt.prefix+"eg"+tt.suffix, gateway.GetName())
			filters := checkGatewayReferences(t, objects)
			if tt.name == "default" {
				checkDefaultXDSFilters(t, filters)
			}
		})
	}
	t.Run("outer overlay of default installation", func(t *testing.T) {
		base, err := filepath.Abs("../../../config/default")
		require.NoError(t, err)
		base, err = filepath.EvalSymlinks(base)
		require.NoError(t, err)
		dir, err := filepath.EvalSymlinks(t.TempDir())
		require.NoError(t, err)
		base, err = filepath.Rel(dir, base)
		require.NoError(t, err)
		overlay := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"+
			"namespace: outer-gateway\nnamePrefix: outer-\nresources:\n- %q\n", base)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(overlay), 0600))
		objects := renderKustomize(t, dir)
		gateway := objectOfKind(t, objects, "Gateway")
		require.Equal(t, "outer-gateway", gateway.GetNamespace())
		require.Equal(t, "outer-aibrix-eg", gateway.GetName())
		checkGatewayReferences(t, objects)
		checkGatewayComponentSettings(t, objects, gateway)
	})
}

func renderKustomize(t *testing.T, dir string) []unstructured.Unstructured {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("kubectl", "kustomize", dir)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	require.NoError(t, err, "%s", stderr.String())
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var objects []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object.Object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if object.GetKind() != "" {
			objects = append(objects, object)
		}
	}
	require.NotEmpty(t, objects)
	return objects
}

func objectOfKind(t *testing.T, objects []unstructured.Unstructured, kind string) unstructured.Unstructured {
	t.Helper()
	var matches []unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() == kind {
			matches = append(matches, object)
		}
	}
	require.Len(t, matches, 1, "expected one %s", kind)
	return matches[0]
}

func objectString(t *testing.T, object map[string]interface{}, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(object, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing %s", strings.Join(fields, "."))
	return value
}

func checkGatewayReferences(t *testing.T, objects []unstructured.Unstructured) map[string]map[string]interface{} {
	t.Helper()
	gateway := objectOfKind(t, objects, "Gateway")
	gatewayClass := objectOfKind(t, objects, "GatewayClass")
	proxy := objectOfKind(t, objects, "EnvoyProxy")
	require.Equal(t, gatewayClass.GetName(), objectString(t, gateway.Object, "spec", "gatewayClassName"))
	require.Equal(t, proxy.GetName(), objectString(t, gatewayClass.Object, "spec", "parametersRef", "name"))
	require.Equal(t, proxy.GetNamespace(), objectString(t, gatewayClass.Object, "spec", "parametersRef", "namespace"))
	checkGatewayServiceReferences(t, objects)
	trafficPolicy := objectOfKind(t, objects, "ClientTrafficPolicy")
	targets, found, err := unstructured.NestedSlice(trafficPolicy.Object, "spec", "targetRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, targets, 1)
	require.Equal(t, gateway.GetName(), objectString(t, targets[0].(map[string]interface{}), "name"))

	routeNames := map[string]bool{}
	filterNames := map[string]bool{}
	for _, object := range objects {
		if object.GetKind() == "HTTPRoute" {
			routeNames[object.GetName()] = true
			parents, _, err := unstructured.NestedSlice(object.Object, "spec", "parentRefs")
			require.NoError(t, err)
			require.NotEmpty(t, parents)
			for _, parent := range parents {
				require.Equal(t, gateway.GetName(), objectString(t, parent.(map[string]interface{}), "name"))
			}
		}
	}
	for _, object := range objects {
		if object.GetKind() != "EnvoyExtensionPolicy" {
			continue
		}
		target := objectString(t, object.Object, "spec", "targetRef", "name")
		require.True(t, routeNames[target], "policy %s targets missing route %s", object.GetName(), target)
		processors, _, err := unstructured.NestedSlice(object.Object, "spec", "extProc")
		require.NoError(t, err)
		for i := range processors {
			name := fmt.Sprintf("envoy.filters.http.ext_proc/envoyextensionpolicy/%s/%s/extproc/%d",
				object.GetNamespace(), object.GetName(), i)
			filterNames[name] = true
		}
	}

	policy := objectOfKind(t, objects, "EnvoyPatchPolicy")
	require.Equal(t, gateway.GetName(), objectString(t, policy.Object, "spec", "targetRef", "name"))
	patches, _, err := unstructured.NestedSlice(policy.Object, "spec", "jsonPatches")
	require.NoError(t, err)
	routeConfig := []byte(`{"name":"gateway/http","virtual_hosts":[{"name":"default","domains":["*"],"routes":[]}]}`)
	// Envoy Gateway v1.2.8 creates missing paths for add operations and round
	// trips through protobuf after every patch. Empty maps disappear there.
	patchOptions := jsonpatch.NewApplyOptions()
	patchOptions.EnsurePathExistsOnAdd = true
	listenerPatched := false
	for _, value := range patches {
		patch := value.(map[string]interface{})
		resourceType := objectString(t, patch, "type")
		if !strings.HasSuffix(resourceType, ".RouteConfiguration") && !strings.HasSuffix(resourceType, ".Listener") {
			continue
		}
		require.Equal(t, gateway.GetNamespace()+"/"+gateway.GetName()+"/http", objectString(t, patch, "name"))
		if strings.HasSuffix(resourceType, ".Listener") {
			listenerPatched = true
			continue
		}
		encoded, err := json.Marshal([]interface{}{patch["operation"]})
		require.NoError(t, err)
		operation, err := jsonpatch.DecodePatch(encoded)
		require.NoError(t, err)
		routeConfig, err = operation.ApplyWithOptions(routeConfig, patchOptions)
		require.NoError(t, err, "invalid route patch: %s", encoded)
		var translated routev3.RouteConfiguration
		require.NoError(t, protojson.Unmarshal(routeConfig, &translated))
		require.NoError(t, translated.Validate())
		routeConfig, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(&translated)
		require.NoError(t, err)
	}
	require.True(t, listenerPatched)
	var patched map[string]interface{}
	require.NoError(t, json.Unmarshal(routeConfig, &patched))
	hosts := patched["virtual_hosts"].([]interface{})
	routes := hosts[0].(map[string]interface{})["routes"].([]interface{})
	require.Len(t, routes, 3)
	usedFilters := map[string]bool{}
	routeFilters := map[string]map[string]interface{}{}
	for _, route := range routes {
		filters, found, err := unstructured.NestedMap(route.(map[string]interface{}), "typed_per_filter_config")
		require.NoError(t, err)
		require.True(t, found)
		require.Len(t, filters, 1)
		routeFilters[objectString(t, route.(map[string]interface{}), "name")] = filters
		for name := range filters {
			require.True(t, filterNames[name], "patch enables missing filter %s", name)
			usedFilters[name] = true
		}
	}
	require.Equal(t, filterNames, usedFilters)
	return routeFilters
}

func checkGatewayServiceReferences(t *testing.T, objects []unstructured.Unstructured) {
	t.Helper()
	services := map[string]bool{}
	metadataPresent := false
	metadataRoutes := map[string]bool{}
	for _, object := range objects {
		switch object.GetKind() {
		case "Service":
			services[object.GetName()] = true
			app, _, err := unstructured.NestedString(object.Object, "spec", "selector", "app")
			require.NoError(t, err)
			metadataPresent = metadataPresent || app == "metadata-service"
		case "EnvoyExtensionPolicy":
			processors, _, err := unstructured.NestedSlice(object.Object, "spec", "extProc")
			require.NoError(t, err)
			if len(processors) == 0 {
				metadataRoutes[objectString(t, object.Object, "spec", "targetRef", "name")] = true
			}
		}
	}
	for _, object := range objects {
		var sections []interface{}
		var err error
		switch object.GetKind() {
		case "HTTPRoute":
			if metadataRoutes[object.GetName()] && !metadataPresent {
				continue // The gateway-only fixture deliberately omits the metadata service.
			}
			sections, _, err = unstructured.NestedSlice(object.Object, "spec", "rules")
		case "EnvoyExtensionPolicy":
			sections, _, err = unstructured.NestedSlice(object.Object, "spec", "extProc")
		default:
			continue
		}
		require.NoError(t, err)
		for _, section := range sections {
			backends, _, err := unstructured.NestedSlice(section.(map[string]interface{}), "backendRefs")
			require.NoError(t, err)
			for _, backend := range backends {
				name := objectString(t, backend.(map[string]interface{}), "name")
				require.True(t, services[name], "%s %s references missing Service %s",
					object.GetKind(), object.GetName(), name)
			}
		}
	}
}

func checkGatewayComponentSettings(
	t *testing.T, objects []unstructured.Unstructured, gateway unstructured.Unstructured,
) {
	t.Helper()
	checked := map[string]bool{}
	for _, object := range objects {
		if object.GetKind() != "Deployment" {
			continue
		}
		containers, _, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		for _, value := range containers {
			container := value.(map[string]interface{})
			name := objectString(t, container, "name")
			if name != "manager" && name != "gateway-plugin" {
				continue
			}
			env, _, err := unstructured.NestedSlice(container, "env")
			require.NoError(t, err)
			settings := map[string]string{}
			for _, entry := range env {
				item := entry.(map[string]interface{})
				key := objectString(t, item, "name")
				if key == "AIBRIX_GATEWAY_NAME" || key == "AIBRIX_GATEWAY_NAMESPACE" {
					require.NotContains(t, settings, key, "duplicate setting")
					settings[key] = objectString(t, item, "value")
				}
			}
			require.Equal(t, gateway.GetNamespace(), settings["AIBRIX_GATEWAY_NAMESPACE"])
			if name == "manager" {
				require.Equal(t, gateway.GetName(), settings["AIBRIX_GATEWAY_NAME"])
			}
			checked[name] = true
		}
	}
	require.Equal(t, map[string]bool{"manager": true, "gateway-plugin": true}, checked)
}

// Compare the resolved filter maps with the real Envoy Gateway golden, keeping
// the pinning-route drift check out of tests that read unresolved source YAML.
func checkDefaultXDSFilters(t *testing.T, filters map[string]map[string]interface{}) {
	t.Helper()
	raw, err := os.ReadFile("../../../pkg/plugins/gateway/testdata/envoy_gateway_v1.2.8_xds.json")
	require.NoError(t, err)
	var golden struct {
		XDS map[string]struct {
			Configs []struct {
				DynamicRouteConfigs []struct {
					RouteConfig struct {
						VirtualHosts []struct {
							Routes []struct {
								Name    string                 `json:"name"`
								Filters map[string]interface{} `json:"typedPerFilterConfig"`
							} `json:"routes"`
						} `json:"virtualHosts"`
					} `json:"routeConfig"`
				} `json:"dynamicRouteConfigs"`
			} `json:"configs"`
		} `json:"xds"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	matched := 0
	for _, config := range golden.XDS["aibrix-system/aibrix-eg"].Configs {
		for _, dynamic := range config.DynamicRouteConfigs {
			for _, host := range dynamic.RouteConfig.VirtualHosts {
				for _, route := range host.Routes {
					if actual, ok := filters[route.Name]; ok {
						require.Equal(t, route.Filters, actual, "pinning route %s differs from generated xDS", route.Name)
						matched++
					}
				}
			}
		}
	}
	require.Equal(t, len(filters), matched)
}
