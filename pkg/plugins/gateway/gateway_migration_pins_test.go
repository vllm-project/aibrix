/*
Copyright 2026 The AIBrix Authors.

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

package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Keep every deployed pin explicit. A missing pin must fail as well as a stale
// pin, including the registry sync sources and destinations. Historical docs,
// the previous-version golden and unrelated sample deployments are not inputs.
var envoyMigrationPins = []struct {
	path           string
	envoy, gateway int
}{
	{"config/dependency/envoy-gateway/kustomization.yaml", 0, 2},
	{"config/gateway/gateway.yaml", 1, 1},
	{"config/overlays/release/envoy_proxy_patch.yaml", 1, 1},
	{"config/overlays/vke/default/envoy_proxy_patch.yaml", 1, 1},
	{"config/overlays/vke/dependency/envoy_gateway_patch.yaml", 0, 1},
	{"config/overlays/vke/dependency/kustomization.yaml", 0, 1},
	{"dist/chart/values.yaml", 1, 1},
	{"dist/chart/stable.yaml", 1, 1},
	{"dist/chart/vke.yaml", 1, 1},
	{"deployment/standalone/docker-compose.yml", 1, 0},
	{"hack/release/sync-dependency-images.sh", 2, 2},
}

// Match the pin locations independently of their version, so a reverted or
// unexpected version is detected rather than disappearing from the matches.
var envoyPinPattern = regexp.MustCompile(`(?:envoyproxy|aibrix)/envoy:([^\s"']+)`)
var gatewayPinPattern = regexp.MustCompile(`(?:(?:envoyproxy|aibrix)/gateway:([^\s"']+)|newTag:\s*([^\s#]+)|envoyproxy/gateway/releases/download/([^/\s]+)/install\.yaml)`)

func checkMigrationPins(raw []byte, envoyCount, gatewayCount int) error {
	for _, component := range []struct {
		name, version string
		pattern       *regexp.Regexp
		count         int
	}{
		{"Envoy", "v1.39.6", envoyPinPattern, envoyCount},
		{"Gateway", "v1.9.4", gatewayPinPattern, gatewayCount},
	} {
		matches := component.pattern.FindAllSubmatch(raw, -1)
		if len(matches) != component.count {
			return fmt.Errorf("%s: got %d pins, want %d", component.name, len(matches), component.count)
		}
		for _, match := range matches {
			for _, group := range match[1:] {
				if len(group) != 0 && string(group) != component.version {
					return fmt.Errorf("%s: got %q, want %q", component.name, group, component.version)
				}
			}
		}
	}
	return nil
}

func TestEnvoyMigrationPins(t *testing.T) {
	for _, pin := range envoyMigrationPins {
		t.Run(pin.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../..", pin.path))
			if err != nil {
				t.Fatal(err)
			}
			if err := checkMigrationPins(raw, pin.envoy, pin.gateway); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnvoyMigrationPinGuardRejectsDrift(t *testing.T) {
	// Mutate every occurrence independently, including the sync destination and
	// second Kubernetes pin. Also remove each occurrence to exercise counts.
	for _, pin := range envoyMigrationPins {
		raw, err := os.ReadFile(filepath.Join("../../..", pin.path))
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range []*regexp.Regexp{envoyPinPattern, gatewayPinPattern} {
			for i, loc := range pattern.FindAllSubmatchIndex(raw, -1) {
				start, end := -1, -1
				for g := 2; g < len(loc); g += 2 {
					if loc[g] >= 0 {
						start, end = loc[g], loc[g+1]
						break
					}
				}
				if start < 0 {
					t.Fatal("pin has no version capture")
				}
				for _, mutation := range []string{"v0.0.0-regression", ""} {
					t.Run(fmt.Sprintf("%s/%d/%s/%s", pin.path, i, pattern.String()[:8], strings.TrimSpace(mutation)), func(t *testing.T) {
						changed := append([]byte{}, raw[:start]...)
						changed = append(changed, mutation...)
						changed = append(changed, raw[end:]...)
						if err := checkMigrationPins(changed, pin.envoy, pin.gateway); err == nil {
							t.Fatal("pin guard accepted a changed or removed version")
						}
					})
				}
			}
		}
	}
}
