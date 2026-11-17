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
	for _, namespace := range []string{"aibrix-system", "gateway-system"} {
		t.Run(namespace, func(t *testing.T) {
			output, err := exec.Command("helm", "template", "aibrix", "../../../dist/chart",
				"--namespace", namespace).CombinedOutput()
			require.NoError(t, err, "%s", output)
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			components := map[string]bool{"aibrix-controller-manager": false, "aibrix-gateway-plugins": false}
			gatewayFound := false
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
					require.Equal(t, "aibrix-eg", gatewayClass.Name)
					require.NotNil(t, gatewayClass.Spec.ParametersRef)
					require.Equal(t, gatewayv1.Namespace(namespace), *gatewayClass.Spec.ParametersRef.Namespace)
					gatewayFound = true
				case "Deployment":
					var deployment appsv1.Deployment
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object, &deployment))
					if _, relevant := components[deployment.Name]; !relevant {
						continue
					}
					count := 0
					for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
						if env.Name == "AIBRIX_GATEWAY_NAMESPACE" {
							require.Equal(t, namespace, env.Value)
							require.Nil(t, env.ValueFrom, "the setting must not follow Pod placement")
							count++
						}
					}
					require.Equal(t, 1, count, deployment.Name)
					components[deployment.Name] = true
				}
			}
			require.True(t, gatewayFound)
			for component, found := range components {
				require.True(t, found, component)
			}
		})
	}
	for _, component := range []string{"controllerManager", "gatewayPlugin"} {
		t.Run(component+" override", func(t *testing.T) {
			output, err := exec.Command("helm", "template", "aibrix", "../../../dist/chart",
				"--set-string", component+".container.envs.AIBRIX_GATEWAY_NAMESPACE=other").CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), "AIBRIX_GATEWAY_NAMESPACE is managed by the chart")
		})
	}
}
