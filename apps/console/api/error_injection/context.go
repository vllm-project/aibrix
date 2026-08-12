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

package error_injection

import (
	"context"
	"encoding/json"

	"k8s.io/klog/v2"
)

// InjectionContextKey is the type for context keys used in error injection
type InjectionContextKey string

// Context key constants for storing injection-related values
const (
	InjectionKeyConfig InjectionContextKey = "injection-config"
)

// WithInjectionContext creates a new context with injection config
func WithInjectionContext(ctx context.Context, cfg *InjectionConfig) context.Context {
	if cfg == nil {
		return ctx
	}
	if cfg.JobID == "" {
		cfgJson, _ := json.Marshal(cfg)
		klog.Errorf("[injection] JobID is empty in config: %s", cfgJson)
		return ctx
	}
	ctx = context.WithValue(ctx, InjectionKeyConfig, cfg)
	return ctx
}

// GetInjectionConfigFromContext retrieves the per-job config from context
// Returns nil if the config is not present or context is nil
func GetInjectionConfigFromContext(ctx context.Context) *InjectionConfig {
	if ctx == nil {
		return nil
	}
	val := ctx.Value(InjectionKeyConfig)
	if val == nil {
		return nil
	}
	cfg, ok := val.(*InjectionConfig)
	if !ok {
		return nil
	}
	return cfg
}
