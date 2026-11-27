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

import "github.com/vllm-project/aibrix/pkg/cache/discovery"

// setModelConfig records the config that discovery supplied for a model. It
// is kept apart from the model's pods, so it neither adds the model nor goes
// away with its last pod.
func (c *Store) setModelConfig(cfg *discovery.ModelConfig) {
	c.modelConfigs.Store(cfg.ModelName, cfg.Config)
}

func (c *Store) deleteModelConfig(cfg *discovery.ModelConfig) {
	c.modelConfigs.Delete(cfg.ModelName)
}

// ModelConfig implements ModelConfigCache.
func (c *Store) ModelConfig(modelName string) (string, bool) {
	return c.modelConfigs.Load(modelName)
}

var _ ModelConfigCache = (*Store)(nil)
