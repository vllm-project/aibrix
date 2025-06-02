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
	"fmt"

	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

var (
	ErrorNotSupport          = fmt.Errorf("not support")
	ErrorSLOFailureRequest   = fmt.Errorf("slo failure request")
	ErrorLoadCapacityReached = fmt.Errorf("load capacity reached")
)

// LoadProvider provides an abstraction to get the utilizatin in terms of specified metrics
type LoadProvider interface {
	// GetUtilization reads utilization of the pod in terms of the metrics
	GetUtilization(ctx *types.RoutingContext, pod *v1.Pod) (float64, error)

	// GetConsumption reads load consumption of the request on specified pod)
	// If ErrorSLOFailureRequest is returned, the request is considered as failed to meet server capacity,
	// so SLO is expected to violate and can be early rejected.
	GetConsumption(ctx *types.RoutingContext, pod *v1.Pod) (float64, error)
}

// CappedLoadProvider provides an abstraction to get the capacity of specified metrics.
type CappedLoadProvider interface {
	LoadProvider

	// Cap returns the capacity of the pod in terms of the metrics
	Cap() float64
}

// CachedLoadProvider reads metrics from the cache
type CachedLoadProvider struct {
	cache      Cache
	metricName string
}

func NewCachedLoadProvider(metricName string) (*CachedLoadProvider, error) {
	c, err := Get()
	if err != nil {
		return nil, err
	}

	return newCachedLoadProvider(c, metricName), nil
}

func newCachedLoadProvider(cache Cache, metricName string) *CachedLoadProvider {
	return &CachedLoadProvider{
		cache:      cache,
		metricName: metricName,
	}
}

func (p *CachedLoadProvider) Cache() Cache {
	return p.cache
}

func (p *CachedLoadProvider) GetUtilization(ctx *types.RoutingContext, pod *v1.Pod) (float64, error) {
	cached, err := p.cache.GetMetricValueByPod(pod.Name, pod.Namespace, p.metricName)
	if err != nil {
		return 0.0, err
	}

	return cached.GetSimpleValue(), err
}

func (p *CachedLoadProvider) GetConsumption(ctx *types.RoutingContext, pod *v1.Pod) (float64, error) {
	return 0.0, ErrorNotSupport
}
