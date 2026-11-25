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
	"io"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayNamespaceManifests(t *testing.T) {
	for _, tt := range []struct {
		name, release, namespace, fullname string
		values                             []string
	}{
		{"default", "aibrix", "aibrix-system", "aibrix", nil},
		{"namespace", "aibrix", "gateway-system", "aibrix", nil},
		{"release", "myrel", "gateway-system", "myrel-aibrix", nil},
		{"fullname override", "myrel", "gateway-system", "custom-gateway",
			[]string{"--set-string", "fullnameOverride=custom-gateway"}},
		{"name override", "myrel", "gateway-system", "myrel-custom",
			[]string{"--set-string", "nameOverride=custom"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"template", tt.release, "../../../dist/chart", "--namespace", tt.namespace}, tt.values...)
			output, err := exec.Command("helm", args...).CombinedOutput()
			require.NoError(t, err, "%s", output)
			checkHelmGatewayReferences(t, output, tt.namespace, tt.fullname)
		})
	}
	for _, component := range []string{"controllerManager", "gatewayPlugin"} {
		for _, key := range []string{"AIBRIX_GATEWAY_NAMESPACE", "AIBRIX_GATEWAY_NAME"} {
			for _, value := range []string{"other", ""} {
				t.Run(component+" override "+key+"="+value, func(t *testing.T) {
					output, err := exec.Command("helm", "template", "aibrix", "../../../dist/chart",
						"--set-string", component+".container.envs."+key+"="+value).CombinedOutput()
					require.Error(t, err)
					require.Contains(t, string(output), key+" is managed by the chart")
				})
			}
		}
	}
}

func checkHelmGatewayReferences(t *testing.T, output []byte, namespace, fullname string) {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	components := map[string]bool{fullname + "-controller-manager": false, fullname + "-gateway-plugins": false}
	gatewayFound, gatewayClassFound := false, false
	for {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch object["kind"] {
		case "GatewayClass":
			var gatewayClass gatewayv1.GatewayClass
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object, &gatewayClass))
			require.Equal(t, fullname+"-eg", gatewayClass.Name)
			require.NotNil(t, gatewayClass.Spec.ParametersRef)
			require.Equal(t, gatewayv1.Namespace(namespace), *gatewayClass.Spec.ParametersRef.Namespace)
			gatewayClassFound = true
		case "Gateway":
			var gateway gatewayv1.Gateway
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object, &gateway))
			require.Equal(t, fullname+"-eg", gateway.Name)
			require.Equal(t, gatewayv1.ObjectName(fullname+"-eg"), gateway.Spec.GatewayClassName)
			gatewayFound = true
		case "Deployment":
			var deployment appsv1.Deployment
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object, &deployment))
			if _, relevant := components[deployment.Name]; !relevant {
				continue
			}
			want := map[string]string{"AIBRIX_GATEWAY_NAMESPACE": namespace}
			if deployment.Name == fullname+"-controller-manager" {
				want["AIBRIX_GATEWAY_NAME"] = fullname + "-eg"
			}
			actual := map[string]string{}
			for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
				if _, managed := want[env.Name]; managed {
					require.NotContains(t, actual, env.Name, "duplicate setting in %s", deployment.Name)
					require.Nil(t, env.ValueFrom, "the setting must not follow Pod placement")
					actual[env.Name] = env.Value
				}
			}
			require.Equal(t, want, actual, deployment.Name)
			components[deployment.Name] = true
		}
	}
	require.True(t, gatewayFound)
	require.True(t, gatewayClassFound)
	for component, found := range components {
		require.True(t, found, component)
	}
}
