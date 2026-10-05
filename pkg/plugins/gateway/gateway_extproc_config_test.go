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
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// The Videos create/status rewriting is only safe if ext_proc holds the whole
// response before any of it reaches the client, and that mode is decided by
// configuration, not by the plugin: Envoy Gateway v1.2.8 never sets ext_proc's
// allow_mode_override, so a per-request ModeOverride is ignored. These tests read
// the deployed manifest, so the code and the configuration it depends on cannot
// drift apart silently.
const gatewayPluginManifestPath = "../../../config/gateway/gateway-plugin/gateway-plugin.yaml"

// kustomizeNamePrefix is applied by config/default, which is why an
// EnvoyExtensionPolicy targets "aibrix-<route name>".
const kustomizeNamePrefix = "aibrix-"

type manifestPathMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type manifestRouteMatch struct {
	Path *manifestPathMatch `json:"path"`
}

type manifestHTTPRoute struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Rules []struct {
			Matches []manifestRouteMatch `json:"matches"`
		} `json:"rules"`
	} `json:"spec"`
}

type manifestExtensionPolicy struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		TargetRef struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"targetRef"`
		ExtProc []struct {
			ProcessingMode struct {
				Request *struct {
					Body string `json:"body"`
				} `json:"request"`
				Response *struct {
					Body string `json:"body"`
				} `json:"response"`
			} `json:"processingMode"`
		} `json:"extProc"`
	} `json:"spec"`
}

type gatewayPluginManifest struct {
	routes   map[string]manifestHTTPRoute
	policies map[string]manifestExtensionPolicy // keyed by the route their targetRef names
	// docs holds every document as free-form YAML, so a test can look for fields
	// the typed structs above deliberately do not know about.
	docs []map[string]interface{}
}

func loadGatewayPluginManifest(t *testing.T) gatewayPluginManifest {
	t.Helper()
	raw, err := os.ReadFile(gatewayPluginManifestPath)
	require.NoError(t, err)

	parsed := gatewayPluginManifest{
		routes:   map[string]manifestHTTPRoute{},
		policies: map[string]manifestExtensionPolicy{},
	}
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(raw), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &kind))

		var free map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &free))
		parsed.docs = append(parsed.docs, free)

		switch kind.Kind {
		case "HTTPRoute":
			var route manifestHTTPRoute
			require.NoError(t, yaml.Unmarshal([]byte(doc), &route))
			parsed.routes[route.Metadata.Name] = route
		case "EnvoyExtensionPolicy":
			var policy manifestExtensionPolicy
			require.NoError(t, yaml.Unmarshal([]byte(doc), &policy))
			parsed.policies[policy.Spec.TargetRef.Name] = policy
		}
	}
	require.NotEmpty(t, parsed.routes)
	return parsed
}

func (m gatewayPluginManifest) policyFor(t *testing.T, routeName string) manifestExtensionPolicy {
	t.Helper()
	policy, ok := m.policies[kustomizeNamePrefix+routeName]
	require.True(t, ok, "no EnvoyExtensionPolicy targets route %q, so its requests would never reach ext_proc", routeName)
	require.Len(t, policy.Spec.ExtProc, 1)
	return policy
}

func (m gatewayPluginManifest) pathMatches(t *testing.T, routeName string) []manifestPathMatch {
	t.Helper()
	route, ok := m.routes[routeName]
	require.True(t, ok, "route %q is missing from the deployed manifest", routeName)

	matches := make([]manifestPathMatch, 0, len(route.Spec.Rules))
	for _, rule := range route.Spec.Rules {
		for _, match := range rule.Matches {
			require.NotNil(t, match.Path)
			matches = append(matches, *match.Path)
		}
	}
	return matches
}

// TestGatewayPluginManifest_VideoJSONResponsesAreBuffered is the configuration
// half of the create/status rewrite: with Buffered response bodies Envoy holds the
// response at its headers until ext_proc has processed the complete body, so an
// upstream create success cannot reach the client before the record is durably
// registered and the backend id replaced. Streamed would put those headers on the
// wire first, leaving no way to answer 503 for a failed registration.
func TestGatewayPluginManifest_VideoJSONResponsesAreBuffered(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)

	assert.Equal(t, []manifestPathMatch{{Type: "PathPrefix", Value: PathVideos}},
		manifest.pathMatches(t, "reserved-router-videos"))

	mode := manifest.policyFor(t, "reserved-router-videos").Spec.ExtProc[0].ProcessingMode
	require.NotNil(t, mode.Request)
	require.NotNil(t, mode.Response)
	assert.Equal(t, "Buffered", mode.Request.Body)
	assert.Equal(t, "Buffered", mode.Response.Body, "the Videos JSON responses must arrive whole and be held until the plugin answers")
}

// TestGatewayPluginManifest_VideoStreamingRoutesStayStreamed keeps the exception
// honest: the rendered video and the synchronous flow must never be buffered into
// the proxy's memory, and neither carries a job id to rewrite.
func TestGatewayPluginManifest_VideoStreamingRoutesStayStreamed(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)

	matches := manifest.pathMatches(t, "reserved-router-videos-streaming")
	require.Len(t, matches, 2)

	var regexMatch string
	for _, match := range matches {
		switch match.Type {
		case "Exact":
			assert.Equal(t, PathVideosSync, match.Value)
		case "RegularExpression":
			regexMatch = match.Value
		default:
			t.Fatalf("a PathPrefix match here would rank below the /v1/videos route and lose its streaming mode: %+v", match)
		}
	}

	// Envoy matches a safe_regex route on the whole path, so the same anchoring is
	// used here to check what this match does and does not cover.
	require.NotEmpty(t, regexMatch)
	contentRe, err := regexp.Compile("^(?:" + regexMatch + ")$")
	require.NoError(t, err)
	assert.True(t, contentRe.MatchString(PathVideos+"/aibrixjob-abc/content"))
	assert.False(t, contentRe.MatchString(PathVideos+"/aibrixjob-abc"), "a status poll must stay on the buffered route")
	assert.False(t, contentRe.MatchString(PathVideos), "create and list must stay on the buffered route")

	mode := manifest.policyFor(t, "reserved-router-videos-streaming").Spec.ExtProc[0].ProcessingMode
	require.NotNil(t, mode.Response)
	assert.Equal(t, "Streamed", mode.Response.Body)
}

// TestGatewayPluginManifest_SharedRouteKeepsStreamingAndDropsVideos checks the
// split actually happened. If /v1/videos were still matched by the shared route,
// the more specific videos route would still win, but a PathPrefix duplicate is
// exactly the kind of leftover that makes route precedence decide which response
// mode the Videos API gets.
func TestGatewayPluginManifest_SharedRouteKeepsStreamingAndDropsVideos(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)

	for _, match := range manifest.pathMatches(t, "reserved-router") {
		assert.NotEqual(t, PathVideos, match.Value, "the Videos API must be routed by its own policy")
	}

	mode := manifest.policyFor(t, "reserved-router").Spec.ExtProc[0].ProcessingMode
	require.NotNil(t, mode.Response)
	assert.Equal(t, "Streamed", mode.Response.Body, "SSE completions must keep being forwarded chunk by chunk")
}

// TestGatewayPluginManifest_NoUnsupportedModeOverrideField guards against the
// tempting one-line fix: EnvoyExtensionPolicy in Envoy Gateway v1.2.8 has no
// allowModeOverride field, so setting it would be dropped by the API server and
// the buffering would silently depend on a per-request override Envoy ignores.
// The walk is over parsed documents rather than the file's text, because the
// manifest's comments name the field precisely to say it is unsupported.
func TestGatewayPluginManifest_NoUnsupportedModeOverrideField(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)
	require.NotEmpty(t, manifest.docs)

	for _, doc := range manifest.docs {
		assert.Empty(t, findModeOverrideFields(doc, ""),
			"the manifest must not configure a mode-override field: Envoy Gateway v1.2.8 has none")
	}
}

// findModeOverrideFields returns the paths of every field in a parsed manifest
// document whose name is a spelling of allowModeOverride, ignoring case and
// separators so allow_mode_override is caught too.
func findModeOverrideFields(node interface{}, path string) []string {
	var found []string
	switch typed := node.(type) {
	case map[string]interface{}:
		for key, value := range typed {
			child := path + "." + key
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if normalized == "allowmodeoverride" {
				found = append(found, child)
			}
			found = append(found, findModeOverrideFields(value, child)...)
		}
	case []interface{}:
		for i, value := range typed {
			found = append(found, findModeOverrideFields(value, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return found
}

// TestGatewayPluginManifest_EveryPolicyTargetsAnExistingRoute catches a typo in a
// targetRef, which would leave a route with no ext_proc at all - for the Videos
// route that means public ids resolving to nothing.
func TestGatewayPluginManifest_EveryPolicyTargetsAnExistingRoute(t *testing.T) {
	manifest := loadGatewayPluginManifest(t)

	for target, policy := range manifest.policies {
		assert.Equal(t, "HTTPRoute", policy.Spec.TargetRef.Kind)
		routeName := strings.TrimPrefix(target, kustomizeNamePrefix)
		assert.Contains(t, manifest.routes, routeName,
			"policy %q targets a route that is not in this manifest", policy.Metadata.Name)
	}
}
