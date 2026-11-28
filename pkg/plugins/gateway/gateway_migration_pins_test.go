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

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// Keep every deployed pin explicit. A missing pin must fail as well as a stale
// pin, including the registry sync sources and destinations. Historical docs,
// the previous-version golden and unrelated sample deployments are not inputs.
var envoyMigrationPins = []struct {
	path           string
	envoy, gateway int
}{
	{"config/dependency/envoy-gateway/kustomization.yaml", 0, 2},
	{"config/dependency/envoy-gateway/envoy_gateway_patch.yaml", 0, 1},
	{"config/gateway/gateway.yaml", 1, 1},
	{"config/overlays/release/envoy_proxy_patch.yaml", 1, 1},
	{"config/overlays/vke/default/envoy_proxy_patch.yaml", 1, 1},
	{"config/overlays/vke/dependency/envoy_gateway_patch.yaml", 0, 1},
	{"config/overlays/vke/dependency/kustomization.yaml", 0, 1},
	{"dist/chart/values.yaml", 1, 1},
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

// dist/chart/stable.yaml and dist/chart/vke.yaml pair released AIBrix images
// (v0.8.0) with a released proxy pair. They must not take the new proxy pins
// until a release ships plugin and controller images qualified behind the new
// proxy: the released v0.8.0 plugin behind Envoy v1.39.6 returns local-reply
// JSON with an empty Content-Type (recorded in the PR's validation update,
// pull request issuecomment-7048011229); current source sets the header in
// normalizeErrorHeaderMutation, introduced by 302db06e (#3382) after v0.8.0,
// but the released v0.8.0 images predate that commit.
func TestReleasedValuesKeepReleasedProxyPins(t *testing.T) {
	for _, tc := range []struct {
		path, envoyPin, gatewayPin string
	}{
		{"dist/chart/stable.yaml", "envoyproxy/envoy:v1.33.2", "envoyproxy/gateway:v1.2.8"},
		{"dist/chart/vke.yaml", "aibrix/envoy:v1.33.2", "aibrix/gateway:v1.2.8"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../..", tc.path))
			if err != nil {
				t.Fatal(err)
			}
			for _, pin := range []string{tc.envoyPin, tc.gatewayPin} {
				if !strings.Contains(string(raw), pin) {
					t.Fatalf("%s: released pin %q not found", tc.path, pin)
				}
			}
			for _, newPin := range []string{"envoy:v1.39.6", "gateway:v1.9.4"} {
				if strings.Contains(string(raw), newPin) {
					t.Fatalf("%s: released values must not take the new proxy pin %q", tc.path, newPin)
				}
			}
		})
	}
}

// ConfigMap data is a YAML string, so its replacement must retain an explicit
// released shutdown image even for Gateways without an EnvoyProxy override.
func TestEnvoyMigrationControllerConfig(t *testing.T) {
	for _, tc := range []struct{ path, image string }{
		{"config/dependency/envoy-gateway/envoy_gateway_patch.yaml", "envoyproxy/gateway:v1.9.4"},
		{"config/overlays/vke/dependency/envoy_gateway_patch.yaml", "aibrix-public-release-cn-beijing.cr.volces.com/aibrix/gateway:v1.9.4"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../..", tc.path))
			require.NoError(t, err)
			var cm struct {
				Data map[string]string `json:"data"`
			}
			require.NoError(t, yaml.Unmarshal(raw, &cm))
			require.Contains(t, cm.Data, "envoy-gateway.yaml")
			var config struct {
				Provider struct {
					Type       string `json:"type"`
					Kubernetes struct {
						ShutdownManager struct {
							Image string `json:"image"`
						} `json:"shutdownManager"`
					} `json:"kubernetes"`
				} `json:"provider"`
				Gateway struct {
					ControllerName string `json:"controllerName"`
				} `json:"gateway"`
				ExtensionAPIs struct {
					EnableEnvoyPatchPolicy bool `json:"enableEnvoyPatchPolicy"`
				} `json:"extensionApis"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(cm.Data["envoy-gateway.yaml"]), &config))
			require.Equal(t, "Kubernetes", config.Provider.Type)
			require.Equal(t, tc.image, config.Provider.Kubernetes.ShutdownManager.Image)
			require.Equal(t, "gateway.envoyproxy.io/gatewayclass-controller", config.Gateway.ControllerName)
			require.True(t, config.ExtensionAPIs.EnableEnvoyPatchPolicy)
		})
	}
}
