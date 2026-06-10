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

package transfer

import (
	"fmt"
	"sort"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]func() KVTransferAgent{}
)

// Register makes a KVTransferAgent factory available under name.
// Called from init() in each agent file.
func Register(name string, factory func() KVTransferAgent) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = factory
}

// Resolve returns a new KVTransferAgent for the given connector type.
// Returns an error for unknown types.
func Resolve(connectorType string) (KVTransferAgent, error) {
	mu.RLock()
	defer mu.RUnlock()
	factory, ok := registry[connectorType]
	if !ok {
		return nil, fmt.Errorf("unknown KV transfer connector type %q (valid: %v)", connectorType, validNames())
	}
	return factory(), nil
}

// ValidAgentNames returns a sorted list of registered connector type names.
func ValidAgentNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	return validNames()
}

func validNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
