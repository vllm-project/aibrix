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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdPaths are the two copies of the ModelClaim CRD that ship. The generated
// config/crd/bases is not among them: it is not tracked, and a cluster is
// given one of these.
var crdPaths = []string{
	filepath.Join("..", "..", "..", "config", "crd", "model", "model.aibrix.ai_modelclaims.yaml"),
	filepath.Join("..", "..", "..", "dist", "chart", "crds", "model.aibrix.ai_modelclaims.yaml"),
}

func modelClaimSpecSchema(t *testing.T, path string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	crd := apiextensionsv1.CustomResourceDefinition{}
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.NotEmpty(t, crd.Spec.Versions)
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec, found := schema.Properties["spec"]
	require.True(t, found)
	return spec
}

// TestTheCRDLeavesPerGPUOptionalButNotHalfDeclared pins the schema down where a
// cluster will enforce it. perGPU itself stays optional, so a claim stored
// before it existed stays valid, and can still be updated and deleted. The
// controller is what refuses to place a claim without it. A declaration that is
// there has to carry both figures, since half of one is a mistake an apply can
// catch.
func TestTheCRDLeavesPerGPUOptionalButNotHalfDeclared(t *testing.T) {
	for _, path := range crdPaths {
		spec := modelClaimSpecSchema(t, path)
		require.NotContains(t, spec.Required, "perGPU", path)

		perGPU, found := spec.Properties["perGPU"]
		require.True(t, found, path)
		require.Contains(t, perGPU.Required, "maximumFootprint", path)
		require.Contains(t, perGPU.Required, "kvFloor", path)
	}
}

func TestModelClaimPrioritySchema(t *testing.T) {
	for _, path := range crdPaths {
		spec := modelClaimSpecSchema(t, path)
		priority, found := spec.Properties["priority"]
		require.True(t, found, path)
		require.Equal(t, "integer", priority.Type, path)
		require.Equal(t, "int32", priority.Format, path)
		require.NotNil(t, priority.Default, path)
		require.JSONEq(t, "0", string(priority.Default.Raw), path)
		require.NotNil(t, priority.Minimum, path)
		require.Equal(t, float64(0), *priority.Minimum, path)

		var immutable bool
		for _, rule := range priority.XValidations {
			if rule.Rule == "self == oldSelf" && rule.Message == "priority is immutable; create a new ModelClaim instead" {
				immutable = true
			}
		}
		require.True(t, immutable, path)
	}
}

func TestHistoricalModelClaimWithoutPriorityDecodesAsZero(t *testing.T) {
	claim := &modelapi.ModelClaim{}
	require.NoError(t, json.Unmarshal([]byte(`{"spec":{"artifactURL":"huggingface://historical/model"}}`), claim))
	require.Equal(t, int32(0), claim.Spec.Priority)
}
