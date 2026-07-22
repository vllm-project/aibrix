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

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/features"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRegisterSchemasIncludesModelClaimAPI(t *testing.T) {
	previous := features.EnabledControllers
	features.EnabledControllers = map[string]bool{
		features.ModelClaimController: true,
	}
	t.Cleanup(func() { features.EnabledControllers = previous })

	testScheme := runtime.NewScheme()
	require.NoError(t, RegisterSchemas(testScheme))
	_, err := testScheme.New(schema.GroupVersionKind{
		Group: modelv1alpha1.GroupVersion.Group, Version: modelv1alpha1.GroupVersion.Version, Kind: "ModelClaim",
	})
	require.NoError(t, err)
}
