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

package routingalgorithms

import (
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
)

// The least-* routers live in the least package. Their algorithm names stay re-exported here so
// callers keep referring to them as routingalgorithms.RouterLeastRequest and so on.
// RouterLeastGpuCache and RouterLeastRequest are deprecated aliases, registered below against
// least-kv-cache and least-request-top-k.
const (
	RouterLeastBusyTime    = least.RouterLeastBusyTime
	RouterLeastGpuCache    = least.RouterLeastGpuCache
	RouterLeastKvCache     = least.RouterLeastKvCache
	RouterLeastLatency     = least.RouterLeastLatency
	RouterLeastRequest     = least.RouterLeastRequest
	RouterLeastRequestTopK = least.RouterLeastRequestTopK
	RouterUtil             = least.RouterUtil
)

// ErrorNoAvailablePod is returned by the load-based routers when no pod is available. It is the
// routeutil value, so errors.Is matches whichever package produced it.
var ErrorNoAvailablePod = routeutil.ErrorNoAvailablePod

func init() {
	Register(RouterLeastBusyTime, least.NewLeastBusyTimeRouter)
	Register(RouterLeastGpuCache, least.NewLeastKvCacheRouter)
	Register(RouterLeastKvCache, least.NewLeastKvCacheRouter)
	Register(RouterLeastLatency, least.NewLeastExpectedLatencyRouter)
	Register(RouterLeastRequest, least.NewLeastRequestTopKRouter)
	Register(RouterLeastRequestTopK, least.NewLeastRequestTopKRouter)
	Register(RouterUtil, least.NewLeastUtilRouter)
}
