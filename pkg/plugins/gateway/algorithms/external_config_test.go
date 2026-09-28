/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package routingalgorithms

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func externalTestEnv(overrides map[string]string) func(string) string {
	values := map[string]string{
		EnvExternalRouterEndpoint:    "http://router.default.svc/v1alpha1/select",
		EnvExternalRouterPolicyMode:  "Advisory",
		EnvExternalRouterFailureMode: "FailOpen",
		EnvExternalRouterFallback:    "least-request",
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

func TestLoadExternalRouterConfig(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		_, err := loadExternalRouterConfigFrom(func(string) string { return "" }, nil)
		require.ErrorIs(t, err, ErrExternalRouterDisabled)
	})
	t.Run("defaults and allowlists", func(t *testing.T) {
		cfg, err := loadExternalRouterConfigFrom(externalTestEnv(map[string]string{
			EnvExternalRouterCandidateAttributes: "topology.kubernetes.io/zone,acceleratorClass",
			EnvExternalRouterCandidateMetrics:    "runningRequests,kvCacheUsage",
			EnvExternalRouterPolicyAttributes:    "tenantTier",
		}), nil)
		require.NoError(t, err)
		require.Equal(t, 10*time.Millisecond, cfg.timeout)
		require.Equal(t, 256, cfg.maxInflight)
		require.Equal(t, int64(256*1024), cfg.maxRequestBytes)
		require.Equal(t, int64(64*1024), cfg.maxResponseBytes)
		require.Equal(t, []string{"acceleratorClass", "topology.kubernetes.io/zone"}, cfg.candidateAttributes)
		require.Contains(t, cfg.candidateMetrics, externalMetricRunningRequests)
		require.Contains(t, cfg.candidateMetrics, externalMetricKVCacheUsage)
	})
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"authoritative fail open", map[string]string{EnvExternalRouterPolicyMode: "Authoritative"}},
		{"advisory missing fallback", map[string]string{EnvExternalRouterFallback: ""}},
		{"self fallback", map[string]string{EnvExternalRouterFallback: "external"}},
		{"unsupported scheme", map[string]string{EnvExternalRouterEndpoint: "file:///tmp/router"}},
		{"userinfo", map[string]string{EnvExternalRouterEndpoint: "http://user:pass@example.com/select"}},
		{"fragment", map[string]string{EnvExternalRouterEndpoint: "http://example.com/select#fragment"}},
		{"zero inflight", map[string]string{EnvExternalRouterMaxInflight: "0"}},
		{"bad duration", map[string]string{EnvExternalRouterTimeout: "soon"}},
		{"duplicate allowlist", map[string]string{EnvExternalRouterCandidateAttributes: "zone,zone"}},
		{"unknown metric", map[string]string{EnvExternalRouterCandidateMetrics: "unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadExternalRouterConfigFrom(externalTestEnv(tt.env), nil)
			require.Error(t, err)
		})
	}
	t.Run("token loaded and trimmed", func(t *testing.T) {
		cfg, err := loadExternalRouterConfigFrom(externalTestEnv(map[string]string{
			EnvExternalRouterAuthTokenFile: "/token",
		}), func(path string) ([]byte, error) {
			require.Equal(t, "/token", path)
			return []byte(" secret\n"), nil
		})
		require.NoError(t, err)
		require.Equal(t, "secret", cfg.bearerToken)
	})
	t.Run("empty token rejected", func(t *testing.T) {
		_, err := loadExternalRouterConfigFrom(externalTestEnv(map[string]string{
			EnvExternalRouterAuthTokenFile: "/token",
		}), func(string) ([]byte, error) { return []byte(" \n"), nil })
		require.Error(t, err)
	})
	t.Run("token with embedded newline rejected", func(t *testing.T) {
		_, err := loadExternalRouterConfigFrom(externalTestEnv(map[string]string{
			EnvExternalRouterAuthTokenFile: "/token",
		}), func(string) ([]byte, error) { return []byte("first\nsecond"), nil })
		require.Error(t, err)
	})
	t.Run("token read error", func(t *testing.T) {
		_, err := loadExternalRouterConfigFrom(externalTestEnv(map[string]string{
			EnvExternalRouterAuthTokenFile: "/token",
		}), func(string) ([]byte, error) { return nil, errors.New("denied") })
		require.Error(t, err)
	})
}
