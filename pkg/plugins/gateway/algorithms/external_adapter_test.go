/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package routingalgorithms

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

type externalAdapterTestCache struct {
	cache.Cache
}

func (*externalAdapterTestCache) RegisterRequestTracker(cache.RequestTracker) {}

func TestExternalRouterInitializationValidatesFallback(t *testing.T) {
	for _, tt := range []struct {
		name      string
		fallback  types.RoutingAlgorithm
		exclusive bool
	}{
		{name: "missing", fallback: "missing-router"},
		{name: "pd", fallback: RouterPD, exclusive: true},
		{name: "slo", fallback: RouterSLOPackLoad, exclusive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvExternalRouterEndpoint, "http://router.default.svc/v1alpha1/select")
			t.Setenv(EnvExternalRouterPolicyMode, "Advisory")
			t.Setenv(EnvExternalRouterFailureMode, "FailClosed")
			t.Setenv(EnvExternalRouterFallback, string(tt.fallback))
			manager := NewRouterManagerWithCache(&externalAdapterTestCache{})
			manager.Init()
			initErr := manager.InitializationError(RouterExternal)
			require.Error(t, initErr)
			if tt.exclusive {
				require.ErrorContains(t, initErr, "exclusive")
			}
			_, valid := manager.Validate(string(RouterExternal))
			require.False(t, valid)
		})
	}
}
