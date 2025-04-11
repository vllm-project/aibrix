/*
Copyright 2024 The Aibrix Team.

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

package vtc

import (
	"context"
	"sync"
)

// TODO: add redis token tracker so that state is shared across plugin instances
type InMemoryTokenTracker struct {
	mu           sync.RWMutex
	tokenCounter map[string]float64
	config       *VTCConfig
}

func NewInMemoryTokenTracker(config *VTCConfig) *InMemoryTokenTracker {
	return &InMemoryTokenTracker{
		tokenCounter: make(map[string]float64),
		config:       config,
	}
}

func (t *InMemoryTokenTracker) GetTokenCount(ctx context.Context, user string) (float64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if user == "" {
		return 0, nil
	}

	if tokens, ok := t.tokenCounter[user]; ok {
		return tokens, nil
	}
	return 0, nil
}

// UpdateTokenCount updates the token count for a user, applying weights.
func (t *InMemoryTokenTracker) UpdateTokenCount(ctx context.Context, user string, inputTokens, outputTokens float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if user == "" {
		return nil
	}

	// Simply add the weighted tokens
	tokens := inputTokens*t.config.InputTokenWeight + outputTokens*t.config.OutputTokenWeight
	t.tokenCounter[user] += tokens

	return nil
}
