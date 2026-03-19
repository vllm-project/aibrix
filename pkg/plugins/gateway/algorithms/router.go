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

package routingalgorithms

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	"k8s.io/klog/v2"
)

const (
	RouterNotSet = ""
)

var (
	ErrInitTimeout           = errors.New("router initialization timeout")
	ErrFallbackNotSupported  = errors.New("router not support fallback")
	ErrFallbackNotRegistered = errors.New("fallback router not registered")
	defaultRM                = NewRouterManager()
)

// RouterItem represents a single routing algorithm and its weight coefficient for multi-router config.
type RouterItem struct {
	Name        string
	Coefficient int // Integer weight coefficient (0 to 1000000)
}

// MultiRouterConfig holds the parsed routing algorithms and their weight coefficients.
type MultiRouterConfig struct {
	Items []RouterItem
}

// ParseMultiRouterConfig parses a multi-router string into a MultiRouterConfig.
// Format example: "prefix-cache:2,least-latency:1,least-request"
// - Weight coefficients must be integers in range [0, 1000000].
// - Default weight coefficient is 1 if omitted.
// - Weight coefficient 0 means the routing algorithm should be skipped.
func ParseMultiRouterConfig(routerStr string) (*MultiRouterConfig, error) {
	if routerStr == "" {
		return nil, errors.New("empty routing algorithm")
	}

	parts := strings.Split(routerStr, ",")
	if len(parts) == 0 {
		return nil, errors.New("invalid routing algorithm format")
	}

	var items []RouterItem

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty algorithm name")
		}

		name := part
		coefInt := 1 // Default weight coefficient is 1

		if strings.Contains(part, ":") {
			subParts := strings.Split(part, ":")
			if len(subParts) != 2 {
				return nil, fmt.Errorf("invalid algorithm format: %s", part)
			}
			name = strings.TrimSpace(subParts[0])
			if name == "" {
				return nil, fmt.Errorf("empty algorithm name in: %s", part)
			}

			coefStr := strings.TrimSpace(subParts[1])
			parsedCoef, err := strconv.Atoi(coefStr)
			if err != nil {
				return nil, fmt.Errorf("invalid weight coefficient in: %s (must be an integer)", part)
			}
			if parsedCoef < 0 || parsedCoef > 1000000 {
				return nil, fmt.Errorf("weight coefficient out of bounds [0, 1000000] in: %s", part)
			}
			coefInt = parsedCoef
		}

		// If weight coefficient is 0, we skip this algorithm entirely
		if coefInt > 0 {
			items = append(items, RouterItem{Name: name, Coefficient: coefInt})
		}
	}

	if len(items) == 0 {
		return nil, errors.New("no valid algorithms found (all weight coefficients were 0)")
	}

	return &MultiRouterConfig{Items: items}, nil
}

type RouterManager struct {
	routerInited      context.Context
	routerDoneInit    context.CancelFunc
	routerFactory     map[types.RoutingAlgorithm]types.RouterProviderFunc
	routerConstructor map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc
	routerMu          sync.RWMutex
}

func NewRouterManager() *RouterManager {
	rm := &RouterManager{}
	rm.routerInited, rm.routerDoneInit = context.WithTimeout(context.Background(), 1*time.Second)
	rm.routerFactory = make(map[types.RoutingAlgorithm]types.RouterProviderFunc)
	rm.routerConstructor = make(map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc)
	return rm
}

// Validate validates if user provided routing routers is supported by gateway
func (rm *RouterManager) Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()
	if _, ok := rm.routerFactory[types.RoutingAlgorithm(algorithms)]; ok {
		return types.RoutingAlgorithm(algorithms), ok
	} else {
		return RouterNotSet, false
	}
}
func Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	return defaultRM.Validate(algorithms)
}

// Select the user provided router provider supported by gateway, no error reported and fallback to random router
// Call Validate before this function to ensure expected behavior.
func (rm *RouterManager) Select(ctx *types.RoutingContext) (types.Router, error) {
	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()
	if provider, ok := rm.routerFactory[ctx.Algorithm]; ok {
		return provider(ctx)
	} else {
		klog.Warningf("Unsupported router strategy: %s, use %s instead.", ctx.Algorithm, RouterRandom)
		return RandomRouter, nil
	}
}
func Select(ctx *types.RoutingContext) (types.Router, error) {
	return defaultRM.Select(ctx)
}

func (rm *RouterManager) Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.routerConstructor[algorithm] = func() types.RouterProviderFunc {
		router, err := constructor()
		if err != nil {
			klog.Errorf("Failed to construct router for %s: %v", algorithm, err)
			return nil
		}
		return func(_ *types.RoutingContext) (types.Router, error) {
			return router, nil
		}
	}
}
func Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	defaultRM.Register(algorithm, constructor)
}

func (rm *RouterManager) RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.routerFactory[algorithm] = provider
	klog.Infof("Registered router for %s", algorithm)
}
func RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	defaultRM.RegisterProvider(algorithm, provider)
}

func (rm *RouterManager) SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	r, ok := router.(types.FallbackRouter)
	if !ok {
		return ErrFallbackNotSupported
	}

	<-rm.routerInited.Done()
	initErr := rm.routerInited.Err()
	if initErr != context.Canceled {
		return fmt.Errorf("router did not initialized: %v", initErr)
	}

	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()

	if provider, ok := rm.routerFactory[fallback]; !ok {
		return ErrFallbackNotRegistered
	} else {
		r.SetFallback(fallback, provider)
	}
	return nil
}
func SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	return defaultRM.SetFallback(router, fallback)
}

func (rm *RouterManager) Init() {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	for algorithm, constructor := range rm.routerConstructor {
		rm.routerFactory[algorithm] = constructor()
		klog.Infof("Registered router for %s", algorithm)
	}
	rm.routerDoneInit()
}
func Init() {
	defaultRM.Init()
}
