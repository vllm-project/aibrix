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

package blacklist

import (
	"strings"

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	ctrlconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
)

// ignoredPod reports whether the blacklist leaves pod alone. Three kinds of pod
// are handled by something that has its own view of their health, and the
// blacklist's pod-level verdict does not fit them:
//
//   - A pod created by a StormService (the storm-service-name label the
//     StormService controller puts on every pod it renders). These are the
//     prefill, decode and encode pods of a disaggregated deployment, whose
//     failures are handled per role and RoleSet by the PD router rather than
//     by excluding one pod.
//   - A prefill, decode or encode pod of a disaggregated deployment by its role
//     label, whoever created it. The PD router pairs the roles within a RoleSet,
//     and excluding every pod of one role would leave it with nothing to pair
//     while ready pods exist: the blacklist's all-excluded fallback only covers
//     the case where nothing is left at all. An encoder is also split off before
//     the blacklist filter on every strategy but PD, so ignoring it here is what
//     makes the PD strategy, which does filter the list an encoder is in, treat
//     it the same way.
//   - A pod that carries a ModelClaim binding (a route.claim.model.aibrix.ai/
//     annotation). A warm pod runs one engine per claimed model, each on its
//     own port, so a verdict about the pod would mix engines: one dead engine
//     would take the healthy ones on the same pod out of routing.
//
// The blacklist never records a verdict for such a pod, never creates an entry
// for it, and the background sweeps neither probe nor score it. Allow therefore
// always admits it. A nil pod is not ignored; callers guard nil themselves.
func ignoredPod(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	if _, ok := pod.Labels[ctrlconstants.StormServiceNameLabelKey]; ok {
		return true
	}
	if isPDRolePod(pod) {
		return true
	}
	for key := range pod.Annotations {
		if strings.HasPrefix(key, constants.ModelClaimPodAnnotationPrefix) {
			return true
		}
	}
	return false
}

// isPDRolePod reports whether pod is the prefill, decode or encode leg of a
// disaggregated (PD, or E+PD) deployment, by the same role label the PD router
// groups pods by.
func isPDRolePod(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	switch pod.Labels[pd.RoleLabel] {
	case pd.RolePrefill, pd.RoleDecode, encode.Role:
		return true
	}
	return false
}

// withoutIgnored drops the pods ignoredPod reports. The result shares pods'
// backing array; pods must not be used afterwards, and must not be a slice
// another reader holds (the sweeps pass a freshly filtered copy).
func withoutIgnored(pods []*v1.Pod) []*v1.Pod {
	kept := pods[:0]
	for _, pod := range pods {
		if !ignoredPod(pod) {
			kept = append(kept, pod)
		}
	}
	return kept
}
