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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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

// TestTheCRDRefusesAClaimThatDeclaresNoPerGPUCost pins the admission rule down
// where a cluster will enforce it. A card carrying one claim that declares
// nothing cannot be accounted for, and is then unusable to every other model on
// it, so the declaration is worth refusing an apply over rather than finding
// out at placement.
func TestTheCRDRefusesAClaimThatDeclaresNoPerGPUCost(t *testing.T) {
	for _, path := range crdPaths {
		spec := modelClaimSpecSchema(t, path)
		require.Contains(t, spec.Required, "perGPU", path)

		perGPU, found := spec.Properties["perGPU"]
		require.True(t, found, path)
		require.Contains(t, perGPU.Required, "maximumFootprint", path)
		require.Contains(t, perGPU.Required, "kvFloor", path)
	}
}
