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

package discovery

import (
	"context"
	"time"
)

// NewTestModelListHealth returns a ModelListHealth whose verification LIST
// is list: nil means the API server matches the cache (both empty), and an
// error fails the LIST. It lets tests outside the package drive a real
// ModelListHealth.
func NewTestModelListHealth(maxAge time.Duration, list func() error) *ModelListHealth {
	h := newModelListHealth(maxAge)
	h.snapshot = func(context.Context) (sourceVersions, error) {
		if err := list(); err != nil {
			return sourceVersions{}, err
		}
		return sourceVersions{pods: map[string]objectVersion{}, adapters: map[string]objectVersion{}}, nil
	}
	return h
}
