/*
Copyright 2024 The Aibrix Team.

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
package cache

import (
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

type PendingLoadProvider struct {
	*CachedLoadProvider
}

func NewPendingLoadProvider() (*PendingLoadProvider, error) {
	cache, err := NewCachedLoadProvider(metrics.RealtimeNormalizedPendings)
	if err != nil {
		return nil, err
	}
	return &PendingLoadProvider{
		CachedLoadProvider: cache,
	}, nil
}

func newPendingLoadProvider(cache Cache) *PendingLoadProvider {
	return &PendingLoadProvider{
		CachedLoadProvider: newCachedLoadProvider(cache, metrics.RealtimeNormalizedPendings),
	}
}

func (p *PendingLoadProvider) Cap() float64 {
	return 1.0
}

func (p *PendingLoadProvider) GetUtilization(ctx *types.RoutingContext, pod *v1.Pod) (float64, error) {
	util, err := p.CachedLoadProvider.GetUtilization(ctx, pod)
	if IsError(err, ErrorTypeMetricNotFound) {
		return 0.0, nil
	} else {
		return util, err
	}
}

func (p *PendingLoadProvider) GetConsumption(ctx *types.RoutingContext, pod *v1.Pod) (float64, error) {
	profile, err := p.Cache().GetModelProfileByPod(pod, ctx.Model)
	if err != nil {
		return 0.0, err
	}

	features, err := ctx.Features()
	if err != nil {
		return 0.0, err
	}

	signature, err := profile.GetSignature(features...)
	if err != nil {
		return 0.0, err
	}

	lambda, err1 := profile.ThroughputRPS(signature...)
	meanLatency, err2 := profile.LatencySeconds(signature...)
	if err1 != nil || err2 != nil {
		return 0.0, err1
	} else if lambda == 0.0 {
		return 0.0, ErrorSLOFailureRequest
	} else {
		return 1.0 / lambda / meanLatency, nil
	}
}
