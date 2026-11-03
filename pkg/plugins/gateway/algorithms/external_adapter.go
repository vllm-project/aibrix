/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package routingalgorithms

import (
	"github.com/vllm-project/aibrix/pkg/cache"
	externalrouter "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/external"
	"github.com/vllm-project/aibrix/pkg/types"
)

const (
	RouterExternal = externalrouter.Algorithm

	EnvExternalRouterEndpoint            = externalrouter.EnvExternalRouterEndpoint
	EnvExternalRouterPolicyMode          = externalrouter.EnvExternalRouterPolicyMode
	EnvExternalRouterFailureMode         = externalrouter.EnvExternalRouterFailureMode
	EnvExternalRouterFallback            = externalrouter.EnvExternalRouterFallback
	EnvExternalRouterTimeout             = externalrouter.EnvExternalRouterTimeout
	EnvExternalRouterMaxInflight         = externalrouter.EnvExternalRouterMaxInflight
	EnvExternalRouterMaxRequestBytes     = externalrouter.EnvExternalRouterMaxRequestBytes
	EnvExternalRouterMaxResponseBytes    = externalrouter.EnvExternalRouterMaxResponseBytes
	EnvExternalRouterFailureThreshold    = externalrouter.EnvExternalRouterFailureThreshold
	EnvExternalRouterOpenDuration        = externalrouter.EnvExternalRouterOpenDuration
	EnvExternalRouterAuthTokenFile       = externalrouter.EnvExternalRouterAuthTokenFile
	EnvExternalRouterCandidateAttributes = externalrouter.EnvExternalRouterCandidateAttributes
	EnvExternalRouterCandidateMetrics    = externalrouter.EnvExternalRouterCandidateMetrics
	EnvExternalRouterPolicyAttributes    = externalrouter.EnvExternalRouterPolicyAttributes
)

var (
	ErrExternalPolicyDenied      = externalrouter.ErrExternalPolicyDenied
	ErrExternalRouterUnavailable = externalrouter.ErrExternalRouterUnavailable
	ErrExternalRouterDisabled    = externalrouter.ErrExternalRouterDisabled
)

func init() {
	Register(RouterExternal, NewExternalRouter)
}

func NewExternalRouter() (types.Router, error) {
	metricCache, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return newExternalRouterWithCacheAndSelector(metricCache, Select)
}

func NewExternalRouterWithCache(metricCache cache.Cache) (types.Router, error) {
	return newExternalRouterWithCacheAndSelector(metricCache, Select)
}

func newExternalRouterWithCacheAndSelector(metricCache cache.Cache, selector func(*types.RoutingContext) (types.Router, error)) (types.Router, error) {
	return externalrouter.NewRouter(metricCache, selector)
}
