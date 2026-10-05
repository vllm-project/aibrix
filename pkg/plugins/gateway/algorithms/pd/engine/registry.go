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

package engine

import (
	"sort"
	"sync"
)

var (
	mu             sync.RWMutex
	registry       = map[string]EngineHandler{}
	defaultHandler = &DefaultHandler{}
)

// Register adds h to the global engine registry.
// Called from init() in each engine file.
func Register(h EngineHandler) {
	mu.Lock()
	defer mu.Unlock()
	registry[h.Name()] = h
}

// Resolve returns the registered EngineHandler for name.
// Returns DefaultHandler for unknown engines so the caller always gets a
// valid handler without needing to check for nil.
func Resolve(name string) EngineHandler {
	mu.RLock()
	defer mu.RUnlock()
	if h, ok := registry[name]; ok {
		return h
	}
	return defaultHandler
}

// ValidEngineNames returns the sorted list of registered engine names.
func ValidEngineNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
