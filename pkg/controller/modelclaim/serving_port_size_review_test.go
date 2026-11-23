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

package modelclaim

// Regression supplied by Zeyu-ZEYU in PR #3560 review:
// https://github.com/vllm-project/aibrix/pull/3560#issuecomment-7101973258

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// warm-1 has a 40 GiB card and warm-2 an 80 GiB one, where the claim other
// serves on 18081. The newcomer needs 60 GiB, so only warm-2 could ever hold
// it. When it asks for 18081, only the port stops it there, so it is not too
// large for every card. Without a port it is placed on warm-2.
func TestReviewABigEnoughCardWhosePortIsTakenIsNotTooSmall(t *testing.T) {
	for _, tt := range []struct {
		name string
		port *int32
	}{{"asks for the taken port", ptr.To[int32](18081)}, {"asks for no port", nil}} {
		t.Run(tt.name, func(t *testing.T) {
			pm := claimWithCost(50<<30, 10<<30)
			pm.UID = types.UID("newcomer-uid")
			pm.Spec.ServingPort = tt.port
			small, smallSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 40<<30)
			larger, largerSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 80<<30)
			other := claimOnPod("other", larger.Name, modelv1alpha1.ModelClaimActive, 8<<30, 2<<30)
			other.UID = types.UID("other-uid")
			engine := engineHolding("other", 1<<30, 2<<30)
			engine.Port = 18081
			engine.ClaimRef = &ModelClaimRef{Namespace: testNamespace, Name: "other", UID: "other-uid"}
			largerSnapshot.Models = []RuntimeSnapshotModel{engine}
			r, runtime := newReconciler(t, pm, small, larger, other)
			runtime.snapshots = map[string]*RuntimeSnapshot{
				small.Status.PodIP:  smallSnapshot,
				larger.Status.PodIP: largerSnapshot,
			}

			reconcileOnce(t, r, pm.Name)

			got := getModel(t, r, pm.Name)
			condition := scheduled(t, r, pm.Name)
			t.Logf("Scheduled %s: %s; instances %v", condition.Reason, condition.Message, got.Status.Instances)
			if tt.port == nil {
				if assert.Len(t, got.Status.Instances, 1) {
					assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
				}
				return
			}
			assert.Empty(t, got.Status.Instances)
			assert.NotEqual(t, constants.ModelClaimReasonTooLargeForAnyCard, condition.Reason,
				"warm-2 could hold the claim once its port is free")
		})
	}
}
