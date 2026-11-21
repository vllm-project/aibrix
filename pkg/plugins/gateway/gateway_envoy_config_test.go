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

package gateway

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// The local and standalone deployments run Envoy from static configs that attach
// ext_proc to the whole listener. The plugin parses every response it sees as a
// completion, so a route to the gateway's own HTTP server or the metadata service
// must opt out of ext_proc, or its response is turned into an error (#3527).
// Nothing else exercises these files, so this test reads them directly.

var envoyConfigPaths = []string{
	"../../../deployment/local/configs/envoy.yaml",
	"../../../deployment/standalone/configs/envoy.yaml",
}

const extProcFilterName = "envoy.filters.http.ext_proc"

// Clusters that serve the gateway's own endpoints or the metadata service.
// Their responses are not completions, so ext_proc must not process them.
var nonInferenceClusters = map[string]bool{
	"gateway_http":     true,
	"metadata_service": true,
	"gateway_metrics":  true,
}

// Clusters that reach an inference engine. ext_proc must stay on for them,
// because that is where the gateway picks the target pod.
var inferenceClusters = map[string]bool{
	"original_destination_cluster": true,
	"vllm_backend":                 true,
}

// envoyRoute holds only the fields this test reads.
type envoyRoute struct {
	Match struct {
		Prefix string `json:"prefix"`
	} `json:"match"`
	Route *struct {
		Cluster string `json:"cluster"`
	} `json:"route"` // nil for direct_response routes
	TypedPerFilterConfig map[string]struct {
		Disabled bool `json:"disabled"`
	} `json:"typed_per_filter_config"`
}

type envoyBootstrap struct {
	StaticResources struct {
		Listeners []struct {
			FilterChains []struct {
				Filters []struct {
					TypedConfig struct {
						RouteConfig struct {
							VirtualHosts []struct {
								Routes []envoyRoute `json:"routes"`
							} `json:"virtual_hosts"`
						} `json:"route_config"`
					} `json:"typed_config"`
				} `json:"filters"`
			} `json:"filter_chains"`
		} `json:"listeners"`
	} `json:"static_resources"`
}

// loadEnvoyRoutes returns every route in every virtual host of the config.
func loadEnvoyRoutes(t *testing.T, path string) []envoyRoute {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg envoyBootstrap
	require.NoError(t, yaml.Unmarshal(raw, &cfg))

	var routes []envoyRoute
	for _, l := range cfg.StaticResources.Listeners {
		for _, fc := range l.FilterChains {
			for _, f := range fc.Filters {
				for _, vh := range f.TypedConfig.RouteConfig.VirtualHosts {
					routes = append(routes, vh.Routes...)
				}
			}
		}
	}
	require.NotEmpty(t, routes, "no routes found in %s", path)
	return routes
}

func TestEnvoyConfigExtProcDisabledOnNonInferenceRoutes(t *testing.T) {
	for _, path := range envoyConfigPaths {
		t.Run(path, func(t *testing.T) {
			routes := loadEnvoyRoutes(t, path)

			checkedNonInference, checkedInference := 0, 0
			for _, r := range routes {
				// direct_response routes have no upstream and never reach ext_proc's response path.
				if r.Route == nil {
					continue
				}
				cluster := r.Route.Cluster
				extProcDisabled := r.TypedPerFilterConfig[extProcFilterName].Disabled

				switch {
				case nonInferenceClusters[cluster]:
					checkedNonInference++
					assert.True(t, extProcDisabled,
						"route %q -> %s must disable %s: its response is not a completion",
						r.Match.Prefix, cluster, extProcFilterName)
				case inferenceClusters[cluster]:
					checkedInference++
					assert.False(t, extProcDisabled,
						"route %q -> %s must keep %s enabled: the gateway routes inference requests there",
						r.Match.Prefix, cluster, extProcFilterName)
				}
			}

			require.Positive(t, checkedNonInference, "no non-inference routes found in %s", path)
			require.Positive(t, checkedInference, "no inference routes found in %s", path)
		})
	}
}
