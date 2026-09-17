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
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// The manifest tests next door assert on the input Envoy Gateway is given. What
// decides whether a Videos response is held, though, is the Envoy configuration
// Envoy Gateway generates from it, and that is more than the HTTPRoutes say: the
// EnvoyPatchPolicy in config/gateway/gateway.yaml prepends the pinning routes
// that every request re-matches after the plugin sets routing-strategy and
// clears the route cache. Which ext_proc filter processes a response is decided
// there, not on the path route the request first arrived on.
//
// These tests read that generated configuration - a real `egctl x translate` of
// the deployed manifests through Envoy Gateway v1.2.8, checked in as a golden -
// and resolve request paths against it the way Envoy would, in both phases.
//
// hack/extproc-xds/README.md regenerates the golden.
const generatedXDSPath = "testdata/envoy_gateway_v1.2.8_xds.json"

// gatewayManifestPath holds the Gateway and the EnvoyPatchPolicy that adds the
// pinning routes.
const gatewayManifestPath = "../../../config/gateway/gateway.yaml"

// extProcFilterPrefix is how Envoy Gateway names the filters it generates for an
// EnvoyExtensionPolicy's extProc entries.
const extProcFilterPrefix = "envoy.filters.http.ext_proc/"

// routingStrategyHeader is the header the plugin sets when it pins a request to
// a pod. Its presence is what makes the pinning routes match on the second pass.
const routingStrategyHeader = "routing-strategy"

type xdsProcessingMode struct {
	RequestBodyMode  string `json:"requestBodyMode"`
	ResponseBodyMode string `json:"responseBodyMode"`
}

type xdsHTTPFilter struct {
	Name string `json:"name"`
	// Disabled is how Envoy Gateway keeps a policy's filter off every route it
	// does not target: the filter is in the chain but inert until a route's
	// typedPerFilterConfig enables it.
	Disabled    bool `json:"disabled"`
	TypedConfig struct {
		Type           string            `json:"@type"`
		ProcessingMode xdsProcessingMode `json:"processingMode"`
	} `json:"typedConfig"`
}

type xdsHeaderMatch struct {
	Name string `json:"name"`
}

type xdsRouteMatch struct {
	Path                string           `json:"path"`
	Prefix              string           `json:"prefix"`
	PathSeparatedPrefix string           `json:"pathSeparatedPrefix"`
	Headers             []xdsHeaderMatch `json:"headers"`
	SafeRegex           *struct {
		Regex string `json:"regex"`
	} `json:"safeRegex"`
}

type xdsRoute struct {
	Name                 string                     `json:"name"`
	Match                xdsRouteMatch              `json:"match"`
	TypedPerFilterConfig map[string]json.RawMessage `json:"typedPerFilterConfig"`
}

type generatedXDS struct {
	filters map[string]xdsHTTPFilter // keyed by filter name, in no particular order
	routes  []xdsRoute               // in the order Envoy evaluates them
}

func loadGeneratedXDS(t *testing.T) generatedXDS {
	t.Helper()
	raw, err := os.ReadFile(generatedXDSPath)
	require.NoError(t, err)

	var dump struct {
		XDS map[string]struct {
			Configs []struct {
				Type             string `json:"@type"`
				DynamicListeners []struct {
					ActiveState struct {
						Listener struct {
							Name               string `json:"name"`
							DefaultFilterChain struct {
								Filters []struct {
									TypedConfig struct {
										HTTPFilters []xdsHTTPFilter `json:"httpFilters"`
									} `json:"typedConfig"`
								} `json:"filters"`
							} `json:"defaultFilterChain"`
						} `json:"listener"`
					} `json:"activeState"`
				} `json:"dynamicListeners"`
				DynamicRouteConfigs []struct {
					RouteConfig struct {
						Name         string `json:"name"`
						VirtualHosts []struct {
							Name   string     `json:"name"`
							Routes []xdsRoute `json:"routes"`
						} `json:"virtualHosts"`
					} `json:"routeConfig"`
				} `json:"dynamicRouteConfigs"`
			} `json:"configs"`
		} `json:"xds"`
	}
	require.NoError(t, json.Unmarshal(raw, &dump))

	parsed := generatedXDS{filters: map[string]xdsHTTPFilter{}}
	for _, gateway := range dump.XDS {
		for _, config := range gateway.Configs {
			for _, listener := range config.DynamicListeners {
				for _, filter := range listener.ActiveState.Listener.DefaultFilterChain.Filters {
					for _, httpFilter := range filter.TypedConfig.HTTPFilters {
						parsed.filters[httpFilter.Name] = httpFilter
					}
				}
			}
			for _, routeConfig := range config.DynamicRouteConfigs {
				for _, virtualHost := range routeConfig.RouteConfig.VirtualHosts {
					parsed.routes = append(parsed.routes, virtualHost.Routes...)
				}
			}
		}
	}
	require.NotEmpty(t, parsed.filters, "the golden has no http filters, so it cannot say anything about ext_proc")
	require.NotEmpty(t, parsed.routes, "the golden has no routes, so it cannot say anything about precedence")
	return parsed
}

// matches replays Envoy's matching for the match types this configuration uses:
// Exact becomes path, RegularExpression becomes a whole-path safe_regex,
// PathPrefix becomes path_separated_prefix (which, unlike a plain prefix, only
// matches on a segment boundary), and the pinning routes additionally require
// the routing-strategy header to be present.
func (m xdsRouteMatch) matches(path string, pinned bool) bool {
	for _, header := range m.Headers {
		if header.Name != routingStrategyHeader || !pinned {
			return false
		}
	}
	switch {
	case m.Path != "":
		return path == m.Path
	case m.SafeRegex != nil:
		return regexp.MustCompile("^(?:" + m.SafeRegex.Regex + ")$").MatchString(path)
	case m.PathSeparatedPrefix != "":
		return path == m.PathSeparatedPrefix || strings.HasPrefix(path, m.PathSeparatedPrefix+"/")
	case m.Prefix != "":
		return strings.HasPrefix(path, m.Prefix)
	}
	return false
}

// resolve returns the route a path lands on and the ext_proc filter that route
// enables. Envoy walks a virtual host's routes in order and takes the first
// match, so the order Envoy Gateway sorted them into is the precedence. pinned
// says whether the plugin has already stamped routing-strategy on the request -
// false is the request as the client sent it, true is the same request after
// ClearRouteCache sends it through the route table a second time.
func (x generatedXDS) resolve(t *testing.T, path string, pinned bool) (xdsRoute, xdsHTTPFilter) {
	t.Helper()
	for _, route := range x.routes {
		if !route.Match.matches(path, pinned) {
			continue
		}
		var enabled []string
		for name := range route.TypedPerFilterConfig {
			if strings.HasPrefix(name, extProcFilterPrefix) {
				enabled = append(enabled, name)
			}
		}
		require.Len(t, enabled, 1, "route %q (%s) must enable exactly one ext_proc filter", route.Name, path)
		filter, ok := x.filters[enabled[0]]
		require.True(t, ok, "route %q enables filter %q, which is not in the filter chain", route.Name, enabled[0])
		return route, filter
	}
	t.Fatalf("no generated route matches %q (pinned=%v)", path, pinned)
	return xdsRoute{}, xdsHTTPFilter{}
}

// routeIndex is where a route sits in Envoy's evaluation order.
func (x generatedXDS) routeIndex(t *testing.T, name string) int {
	t.Helper()
	for i, route := range x.routes {
		if route.Name == name {
			return i
		}
	}
	t.Fatalf("no generated route is named %q", name)
	return -1
}

// TestGeneratedXDS_VideoJSONResponsesAreBufferedInBothPhases is the create
// guarantee checked against what Envoy is actually told. A Videos JSON request
// is matched twice: once as the client sent it, and once after the plugin pins
// it with routing-strategy and clears the route cache. Both passes have to land
// on the Videos filter, because the route in effect at the second pass is the
// one Envoy uses for the response - and that filter buffers the response body,
// so Envoy holds the response until ext_proc has seen the whole body and the
// plugin has rewritten the backend job id, or answered 503 because the record
// was never durably registered.
func TestGeneratedXDS_VideoJSONResponsesAreBufferedInBothPhases(t *testing.T) {
	xds := loadGeneratedXDS(t)

	for _, path := range []string{
		PathVideos,                    // create and list
		PathVideos + "/aibrixjob-abc", // status poll and delete
		PathVideos + "/video-42",      // the rewritten, backend-id form of the same call
	} {
		for _, pinned := range []bool{false, true} {
			_, filter := xds.resolve(t, path, pinned)
			assert.Contains(t, filter.Name, "gateway-plugins-videos-extension-policy",
				"path %s (pinned=%v) must stay on the Videos ext_proc filter: a response encoded by a different filter reaches the plugin as a second, contextless stream", path, pinned)
			assert.Equal(t, "BUFFERED", filter.TypedConfig.ProcessingMode.ResponseBodyMode,
				"%s (pinned=%v) must be held whole: its backend job id is rewritten before any byte reaches the client", path, pinned)
			assert.Equal(t, "BUFFERED", filter.TypedConfig.ProcessingMode.RequestBodyMode, "path %s (pinned=%v)", path, pinned)
		}
	}
}

// TestGeneratedXDS_VideoStreamingPathsStayStreamed is the other half: the two
// endpoints that return bytes rather than a job id must not be accumulated in
// the proxy, in either phase.
func TestGeneratedXDS_VideoStreamingPathsStayStreamed(t *testing.T) {
	xds := loadGeneratedXDS(t)

	for _, path := range []string{
		PathVideosSync,
		PathVideos + "/aibrixjob-abc/content",
		PathVideos + "/video-42/content", // the rewritten form
	} {
		for _, pinned := range []bool{false, true} {
			_, filter := xds.resolve(t, path, pinned)
			assert.Contains(t, filter.Name, "gateway-plugins-videos-streaming-extension-policy", "path %s (pinned=%v)", path, pinned)
			assert.Equal(t, "STREAMED", filter.TypedConfig.ProcessingMode.ResponseBodyMode,
				"%s (pinned=%v) carries no job id to rewrite and must never be buffered into the proxy", path, pinned)
		}
	}
}

// TestGeneratedXDS_PinnedRoutePrecedence pins the orderings the two tests above
// depend on. Every pinning route matches "/" as far as the path is concerned
// once the header is there, so only the order they were prepended in keeps a
// Videos response off the shared filter - and /v1/videos/sync would be swallowed
// by the JSON pattern's single-segment form if the streaming route did not come
// first.
func TestGeneratedXDS_PinnedRoutePrecedence(t *testing.T) {
	xds := loadGeneratedXDS(t)

	streaming := xds.routeIndex(t, "original_route_videos_streaming")
	videos := xds.routeIndex(t, "original_route_videos")
	shared := xds.routeIndex(t, "original_route")

	assert.Less(t, streaming, videos, "/v1/videos/sync also matches the JSON pinning route, so the streaming one has to be evaluated first")
	assert.Less(t, videos, shared, "the catch-all pinning route would otherwise take every pinned Videos response onto the shared, streamed filter")

	// And the pinning routes have to precede the path routes, or a pinned request
	// would go back to its path route and to the normal backend cluster instead of
	// the original-destination one.
	for _, route := range xds.routes[:shared+1] {
		assert.NotContains(t, route.Name, "httproute/", "route %q sits above the pinning routes", route.Name)
	}
}

// TestGeneratedXDS_SharedRouteKeepsStreaming guards the rest of the API: the
// split was introduced for the Videos JSON responses alone, and an SSE completion
// that got buffered would be delivered in one lump at the end of generation.
func TestGeneratedXDS_SharedRouteKeepsStreaming(t *testing.T) {
	xds := loadGeneratedXDS(t)

	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/video/generations"} {
		for _, pinned := range []bool{false, true} {
			_, filter := xds.resolve(t, path, pinned)
			assert.Contains(t, filter.Name, "gateway-plugins-extension-policy", "path %s (pinned=%v)", path, pinned)
			assert.Equal(t, "STREAMED", filter.TypedConfig.ProcessingMode.ResponseBodyMode, "path %s (pinned=%v)", path, pinned)
		}
	}
}

// TestGeneratedXDS_ExtProcFiltersAreRouteScoped explains why resolving a path to
// a single filter is meaningful at all: every generated ext_proc filter sits in
// the chain disabled, and a route turns exactly one of them on. Were one enabled
// chain-wide, a Videos response would pass through two ext_proc filters with
// opposite body modes, the plugin would see the same response twice, and the
// buffering would depend on filter order instead.
func TestGeneratedXDS_ExtProcFiltersAreRouteScoped(t *testing.T) {
	xds := loadGeneratedXDS(t)

	extProcFilters := 0
	for name, filter := range xds.filters {
		if !strings.HasPrefix(name, extProcFilterPrefix) {
			continue
		}
		extProcFilters++
		assert.True(t, filter.Disabled, "ext_proc filter %q is enabled for every route", name)
	}
	assert.Equal(t, 3, extProcFilters, "the shared, videos and videos-streaming policies each generate one filter")
}

// patchedRoute is the route an EnvoyPatchPolicy JSONPatch inserts.
type patchedRoute struct {
	Name                 string                     `json:"name"`
	TypedPerFilterConfig map[string]json.RawMessage `json:"typed_per_filter_config"`
}

// loadPatchedRoutes reads the routes the EnvoyPatchPolicy adds to the generated
// RouteConfiguration. They are ordinary Envoy config, not Gateway API, so the
// manifest test next door cannot see them at all.
func loadPatchedRoutes(t *testing.T) map[string]patchedRoute {
	t.Helper()
	raw, err := os.ReadFile(gatewayManifestPath)
	require.NoError(t, err)

	routes := map[string]patchedRoute{}
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(raw), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var policy struct {
			Kind string `json:"kind"`
			Spec struct {
				JSONPatches []struct {
					Type      string `json:"type"`
					Operation struct {
						Path string `json:"path"`
						// Raw, because the same list also patches a listener with a
						// bare boolean value.
						Value json.RawMessage `json:"value"`
					} `json:"operation"`
				} `json:"jsonPatches"`
			} `json:"spec"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &policy))
		if policy.Kind != "EnvoyPatchPolicy" {
			continue
		}
		for _, patch := range policy.Spec.JSONPatches {
			if !strings.HasSuffix(patch.Type, "route.v3.RouteConfiguration") {
				continue
			}
			var route patchedRoute
			require.NoError(t, yaml.Unmarshal(patch.Operation.Value, &route))
			require.NotEmpty(t, route.Name)
			routes[route.Name] = route
		}
	}
	require.NotEmpty(t, routes, "no EnvoyPatchPolicy route patches found, so pinned requests would not reach the original-destination cluster")
	return routes
}

// TestGeneratedXDS_MatchesDeployedManifest is the drift check. The golden is a
// snapshot: editing a manifest without regenerating it would leave these tests
// asserting about configuration nobody deploys. Comparing the two directly means
// that shows up as a failure here rather than as buffering that quietly stopped
// happening in a cluster.
func TestGeneratedXDS_MatchesDeployedManifest(t *testing.T) {
	xds := loadGeneratedXDS(t)
	manifest := loadGatewayPluginManifest(t)

	bodyMode := map[string]string{"Buffered": "BUFFERED", "Streamed": "STREAMED", "": "NONE"}
	for routeName, policy := range manifest.policies {
		if len(policy.Spec.ExtProc) == 0 {
			continue // skip-ext-proc has no extProc entry, and generates no filter
		}
		// The deployed names carry config/default's namePrefix, which is what the
		// golden was generated from.
		name := extProcFilterPrefix + "envoyextensionpolicy/aibrix-system/" +
			kustomizeNamePrefix + policy.Metadata.Name + "/extproc/0"
		filter, ok := xds.filters[name]
		require.True(t, ok, "policy %q (targeting %q) generated no filter in the golden - regenerate it, see hack/extproc-xds/README.md", policy.Metadata.Name, routeName)

		mode := policy.Spec.ExtProc[0].ProcessingMode
		require.NotNil(t, mode.Request)
		require.NotNil(t, mode.Response)
		assert.Equal(t, bodyMode[mode.Request.Body], filter.TypedConfig.ProcessingMode.RequestBodyMode, "policy %q", policy.Metadata.Name)
		assert.Equal(t, bodyMode[mode.Response.Body], filter.TypedConfig.ProcessingMode.ResponseBodyMode, "policy %q", policy.Metadata.Name)
	}

	// The path matches have to line up too: a new match in the manifest that the
	// golden has never seen is precisely the drift that hides a precedence change.
	generated := map[string]bool{}
	byName := map[string]xdsRoute{}
	for _, route := range xds.routes {
		byName[route.Name] = route
		switch {
		case route.Match.Path != "":
			generated["Exact:"+route.Match.Path] = true
		case route.Match.SafeRegex != nil:
			generated["RegularExpression:"+route.Match.SafeRegex.Regex] = true
		case route.Match.PathSeparatedPrefix != "":
			generated["PathPrefix:"+route.Match.PathSeparatedPrefix] = true
		}
	}
	for _, routeName := range []string{"reserved-router", "reserved-router-videos", "reserved-router-videos-streaming"} {
		for _, match := range manifest.pathMatches(t, routeName) {
			assert.Contains(t, generated, match.Type+":"+match.Value,
				"route %q matches %s %s, which the golden xDS does not - regenerate it, see hack/extproc-xds/README.md", routeName, match.Type, match.Value)
		}
	}

	// And so do the patched pinning routes, which decide the response phase.
	for name, patched := range loadPatchedRoutes(t) {
		route, ok := byName[name]
		require.True(t, ok, "patched route %q is missing from the golden - regenerate it, see hack/extproc-xds/README.md", name)
		assert.Equal(t, keysOf(patched.TypedPerFilterConfig), keysOf(route.TypedPerFilterConfig),
			"patched route %q enables different filters than the golden has it enabling", name)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
