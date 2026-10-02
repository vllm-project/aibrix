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

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestModelWarmupCustomActionJSONRoundTrip(t *testing.T) {
	raw := []byte(`{
  "apiVersion": "model.aibrix.ai/v1alpha1",
  "kind": "ModelWarmup",
  "spec": {
    "targets": [{"nodes": {"names": ["node-a"]}}],
    "custom": {
      "initContainers": [{"name": "init", "image": "busybox"}],
      "containers": [{"name": "sidecar", "image": "busybox"}],
      "volumes": [{"name": "scratch", "emptyDir": {}}],
      "imagePullSecrets": [{"name": "registry-secret"}]
    }
  }
}`)

	var warmup ModelWarmup
	if err := json.Unmarshal(raw, &warmup); err != nil {
		t.Fatalf("unmarshal raw ModelWarmup: %v", err)
	}

	encoded, err := json.Marshal(warmup)
	if err != nil {
		t.Fatalf("marshal ModelWarmup: %v", err)
	}

	var object map[string]interface{}
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal encoded ModelWarmup: %v", err)
	}

	spec, ok := object["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("encoded ModelWarmup spec has type %T, want object", object["spec"])
	}

	custom, ok := spec["custom"].(map[string]interface{})
	if !ok {
		t.Fatalf("encoded ModelWarmup custom has type %T, want object", spec["custom"])
	}

	for _, field := range []string{"initContainers", "containers", "volumes", "imagePullSecrets"} {
		items, ok := custom[field].([]interface{})
		if !ok {
			t.Fatalf("encoded custom.%s has type %T, want array", field, custom[field])
		}
		if len(items) != 1 {
			t.Errorf("encoded custom.%s length = %d, want 1", field, len(items))
		}
	}
}
