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
	"strings"
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
		Prefix          string `json:"prefix"`
		Headers         []any  `json:"headers"`
		QueryParameters []any  `json:"query_parameters"`
		RuntimeFraction any    `json:"runtime_fraction"`
		CaseSensitive   *bool  `json:"case_sensitive"`
		Grpc            any    `json:"grpc"`
	} `json:"match"`
	Route *struct {
		Cluster string `json:"cluster"`
	} `json:"route"` // nil for direct_response routes
	TypedPerFilterConfig map[string]struct {
		Disabled bool `json:"disabled"`
	} `json:"typed_per_filter_config"`
}

func (r envoyRoute) hasExtraMatchConstraints() bool {
	return len(r.Match.Headers) > 0 ||
		len(r.Match.QueryParameters) > 0 ||
		r.Match.RuntimeFraction != nil ||
		r.Match.Grpc != nil
}

// prefixMatches reports whether r's prefix matches target, honoring Envoy's
// case_sensitive setting (default true).
func (r envoyRoute) prefixMatches(target string) bool {
	if r.Match.CaseSensitive != nil && !*r.Match.CaseSensitive {
		return strings.HasPrefix(strings.ToLower(target), strings.ToLower(r.Match.Prefix))
	}
	return strings.HasPrefix(target, r.Match.Prefix)
}

type envoyVirtualHost struct {
	Name   string       `json:"name"`
	Routes []envoyRoute `json:"routes"`
}

type envoyBootstrap struct {
	StaticResources struct {
		Listeners []struct {
			FilterChains []struct {
				Filters []struct {
					TypedConfig struct {
						RouteConfig struct {
							VirtualHosts []envoyVirtualHost `json:"virtual_hosts"`
						} `json:"route_config"`
					} `json:"typed_config"`
				} `json:"filters"`
			} `json:"filter_chains"`
		} `json:"listeners"`
	} `json:"static_resources"`
}

// loadEnvoyVirtualHosts returns every virtual host declared in the config.
func loadEnvoyVirtualHosts(t *testing.T, path string) []envoyVirtualHost {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg envoyBootstrap
	require.NoError(t, yaml.Unmarshal(raw, &cfg))

	var vhs []envoyVirtualHost
	for _, l := range cfg.StaticResources.Listeners {
		for _, fc := range l.FilterChains {
			for _, f := range fc.Filters {
				vhs = append(vhs, f.TypedConfig.RouteConfig.VirtualHosts...)
			}
		}
	}
	require.NotEmpty(t, vhs, "no virtual hosts found in %s", path)
	return vhs
}

// loadEnvoyRoutes returns every route in every virtual host of the config.
func loadEnvoyRoutes(t *testing.T, path string) []envoyRoute {
	t.Helper()
	var routes []envoyRoute
	for _, vh := range loadEnvoyVirtualHosts(t, path) {
		routes = append(routes, vh.Routes...)
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
				// request headers still go through ext_proc unless explicitly opted out per-route.
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

// TestEnvoyConfigNoShadowedPrefixRoutes verifies per virtual host that no
// prefix route is shadowed by an earlier unconstrained route with a shorter
// (or equal) prefix. Envoy evaluates routes in declaration order and picks the
// first match, so placing prefix "/health" before prefix "/healthz" routes
// "/healthz" to vllm_backend instead of returning Envoy's direct_response (#3536).
func TestEnvoyConfigNoShadowedPrefixRoutes(t *testing.T) {
	for _, path := range envoyConfigPaths {
		t.Run(path, func(t *testing.T) {
			for _, vh := range loadEnvoyVirtualHosts(t, path) {
				for i, later := range vh.Routes {
					if !assert.NotEmpty(t, later.Match.Prefix,
						"route %d in virtual host %q of %s has no prefix match: extend envoyRoute to decode it", i, vh.Name, path) {
						continue
					}
					for j := 0; j < i; j++ {
						earlier := vh.Routes[j]
						if earlier.hasExtraMatchConstraints() || earlier.Match.Prefix == "" {
							continue
						}
						assert.Falsef(t, earlier.prefixMatches(later.Match.Prefix),
							"route %d (prefix %q) in virtual host %q of %s is shadowed by earlier route %d (prefix %q)",
							i, later.Match.Prefix, vh.Name, path, j, earlier.Match.Prefix)
					}
				}
			}
		})
	}
}

// TestEnvoyConfigStandaloneHealthzRoute simulates first-match prefix routing on
// the standalone config for GET /healthz and verifies it lands on a direct_response
// route with ext_proc disabled so /healthz reports Envoy's own readiness even when
// both vllm_backend and gateway-plugins are unavailable (#3536).
func TestEnvoyConfigStandaloneHealthzRoute(t *testing.T) {
	vhs := loadEnvoyVirtualHosts(t, "../../../deployment/standalone/configs/envoy.yaml")
	require.Len(t, vhs, 1)

	var matched *envoyRoute
	for i := range vhs[0].Routes {
		r := &vhs[0].Routes[i]
		if r.prefixMatches("/healthz") {
			// Fail rather than skipping if a prefix-matching route adds header/query/runtime
			// constraints (e.g. present_match or invert_match) that this loop does not evaluate.
			require.Falsef(t, r.hasExtraMatchConstraints(),
				"route %d (prefix %q) matches /healthz with extra constraints this simulation cannot evaluate",
				i, r.Match.Prefix)
			matched = r
			break
		}
	}

	require.NotNil(t, matched, "no route matched /healthz")
	assert.Equal(t, "/healthz", matched.Match.Prefix)
	assert.Nil(t, matched.Route, "/healthz should return Envoy direct_response rather than proxying upstream")
	assert.True(t, matched.TypedPerFilterConfig[extProcFilterName].Disabled,
		"/healthz should disable ext_proc so Envoy reports healthy even when the gateway plugin is down")
}
