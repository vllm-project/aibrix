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

// Whether ext_proc must be disabled on routes to each cluster. The gateway's own
// endpoints and the metadata service do not return completions, so ext_proc must
// skip them; inference clusters keep it, because that is where the gateway picks
// the target pod. A route to a cluster missing here fails the test.
var extProcDisabledByCluster = map[string]bool{
	"gateway_http":                 true,
	"metadata_service":             true,
	"gateway_metrics":              true,
	"original_destination_cluster": false,
	"vllm_backend":                 false,
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

			seen := map[bool]bool{} // which kinds of route were checked
			for _, r := range routes {
				// Only prefix matches are decoded; another match kind (path, safe_regex, ...)
				// needs this test updated before it can be checked.
				if !assert.NotEmpty(t, r.Match.Prefix, "route without a prefix match in %s: extend envoyRoute to decode it", path) {
					continue
				}
				// direct_response routes have no upstream response for ext_proc to parse; their
				// request headers still go through ext_proc, which this test does not cover.
				if r.Route == nil {
					continue
				}
				cluster := r.Route.Cluster
				want, known := extProcDisabledByCluster[cluster]
				if !assert.True(t, known, "route %q -> unknown cluster %q: add it to extProcDisabledByCluster",
					r.Match.Prefix, cluster) {
					continue
				}
				seen[want] = true
				assert.Equal(t, want, r.TypedPerFilterConfig[extProcFilterName].Disabled,
					"route %q -> %s: %s disabled should be %v", r.Match.Prefix, cluster, extProcFilterName, want)
			}

			require.True(t, seen[true] && seen[false], "%s must have both inference and non-inference routes", path)
		})
	}
}
