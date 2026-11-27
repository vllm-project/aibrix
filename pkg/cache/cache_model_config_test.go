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

package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestModelConfigFromStaticProvider(t *testing.T) {
	const profiles = `{"profiles": {"default": {"routingStrategy": "least-request"}}}`
	path := filepath.Join(t.TempDir(), "endpoints.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  - name: configured-model
    workers: ["127.0.0.1:8000"]
    config: '`+profiles+`'
  - name: plain-model
    workers: ["127.0.0.1:8001"]
`), 0o600))

	store := NewForTest()
	stopCh := make(chan struct{})
	defer close(stopCh)
	require.NoError(t, initDiscoveryProvider(store, discovery.NewStaticProvider(path), stopCh))

	var c Cache = store
	provider, ok := c.(ModelConfigCache)
	require.True(t, ok, "the store implements ModelConfigCache")
	cfg, found := provider.ModelConfig("configured-model")
	assert.True(t, found)
	assert.Equal(t, profiles, cfg)
	_, found = provider.ModelConfig("plain-model")
	assert.False(t, found, "a model without config has none")
	assert.ElementsMatch(t, []string{"configured-model", "plain-model"}, store.ListModels())
}

func TestModelConfigDiscoveryEvents(t *testing.T) {
	annotated := testPod("p1", "ns", "k8s-model", 1)
	annotated.Annotations = map[string]string{
		constants.ModelAnnoConfig: `{"profiles": {"default": {"routingStrategy": "random"}}}`,
	}
	first := &discovery.ModelConfig{ModelName: "static-model", Config: `{"profiles": {"a": {}}}`}
	second := &discovery.ModelConfig{ModelName: "static-model", Config: `{"profiles": {"b": {}}}`}

	tests := []struct {
		name       string
		events     []discovery.WatchEvent
		model      string
		wantConfig string
		wantFound  bool
	}{
		{
			name:       "add",
			events:     []discovery.WatchEvent{{Type: discovery.EventAdd, Object: first}},
			model:      "static-model",
			wantConfig: first.Config,
			wantFound:  true,
		},
		{
			name: "update replaces the config",
			events: []discovery.WatchEvent{
				{Type: discovery.EventAdd, Object: first},
				{Type: discovery.EventUpdate, Object: second, OldObject: first},
			},
			model:      "static-model",
			wantConfig: second.Config,
			wantFound:  true,
		},
		{
			name: "delete removes the config",
			events: []discovery.WatchEvent{
				{Type: discovery.EventAdd, Object: first},
				{Type: discovery.EventDelete, Object: first},
			},
			model: "static-model",
		},
		{
			// Kubernetes discovery delivers pods only; their annotation stays
			// the source and is not copied into the model config.
			name:   "annotated pod adds no model config",
			events: []discovery.WatchEvent{{Type: discovery.EventAdd, Object: annotated}},
			model:  "k8s-model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewForTest()
			stopCh := make(chan struct{})
			defer close(stopCh)
			require.NoError(t, initDiscoveryProvider(store, &mockProvider{events: tt.events}, stopCh))

			cfg, found := store.ModelConfig(tt.model)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantConfig, cfg)
		})
	}
}
