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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Run the shipped CEL programs against objects, rather than asserting on the
// wording of the rules. Removing a guard must admit a conflicting policy here.
func TestWakePolicyCRDRejectsConflictingSleepPolicy(t *testing.T) {
	for _, path := range crdPaths {
		t.Run(path, func(t *testing.T) {
			spec := modelClaimSpecSchema(t, path)
			_, found := spec.Properties["residencyPolicy"]
			require.True(t, found, "residency policy must survive API server pruning")
			internal := &apiextensions.JSONSchemaProps{}
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&spec, internal, nil))
			structural, err := schema.NewStructural(internal)
			require.NoError(t, err)
			validator := cel.NewValidator(structural, false, 1000000)
			require.NotNil(t, validator)
			for _, tt := range []struct {
				name, policy string
				valid        bool
			}{
				{"empty", `{}`, true},
				{"pool default", `{"sleepPolicy":{"mode":"PoolDefault"},"wakePolicy":{"mode":"OnDemand"}}`, true},
				{"never default", `{"sleepPolicy":{"mode":"Never"}}`, true},
				{"ensure awake", `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"EnsureAwake"}}`, true},
				{"never on demand", `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"OnDemand"}}`, true},
				{"idle oscillation", `{"sleepPolicy":{"mode":"AfterIdle","idleTimeout":"15m"},"wakePolicy":{"mode":"EnsureAwake"}}`, false},
				{"inherited oscillation", `{"sleepPolicy":{"mode":"PoolDefault"},"wakePolicy":{"mode":"EnsureAwake"}}`, false},
				{"implicit inherited oscillation", `{"wakePolicy":{"mode":"EnsureAwake"}}`, false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					var policy map[string]interface{}
					require.NoError(t, json.Unmarshal([]byte(tt.policy), &policy))
					obj := map[string]interface{}{"engine": "vllm", "residencyPolicy": policy}
					errs, _ := validator.Validate(context.Background(), field.NewPath("spec"), structural, obj, nil, 10000000)
					if tt.valid {
						require.Empty(t, errs)
					} else {
						require.NotEmpty(t, errs)
					}
				})
			}
		})
	}
}

func TestWakePolicyCRDRejectsUnsupportedModes(t *testing.T) {
	for _, path := range crdPaths {
		spec := modelClaimSpecSchema(t, path)
		residency, found := spec.Properties["residencyPolicy"]
		require.True(t, found)
		internal := &apiextensions.JSONSchemaProps{}
		require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&residency, internal, nil))
		validator, _, err := validation.NewSchemaValidator(internal)
		require.NoError(t, err)
		for _, tt := range []struct {
			policy string
			valid  bool
		}{
			{`{}`, true},
			{`{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"EnsureAwake"}}`, true},
			{`{"wakePolicy":{"mode":"OnDemand"}}`, true},
			{`{"wakePolicy":{"mode":"Scheduled"}}`, false},
			{`{"wakePolicy":{"mode":"AlwaysOn"}}`, false},
			{`{"wakePolicy":{}}`, false},
			{`{"wakePolicy":{"mode":true}}`, false},
			{`{"sleepPolicy":{"mode":"AfterIdle","idleTimeout":"15m"},"wakePolicy":{"mode":"OnDemand"}}`, false},
			{`{"sleepPolicy":{}}`, false},
		} {
			var policy map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(tt.policy), &policy))
			errs := validation.ValidateCustomResource(field.NewPath("spec", "residencyPolicy"), policy, validator)
			if tt.valid {
				require.Empty(t, errs, path, tt.policy)
			} else {
				require.NotEmpty(t, errs, path, tt.policy)
			}
		}
	}
}
