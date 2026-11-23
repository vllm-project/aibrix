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

// Regression cases supplied by Zeyu-ZEYU in PR #3560 review:
// https://github.com/vllm-project/aibrix/pull/3560#issuecomment-7100468513

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func reviewPortClaim(t *testing.T, r *ModelClaimReconciler, name string, port *int32, created metav1.Time) *modelv1alpha1.ModelClaim {
	t.Helper()
	claim := withFinalizer(claimOnPod(name, "", modelv1alpha1.ModelClaimActive, 300, 100))
	claim.UID = types.UID(name + "-uid")
	claim.Status.Instances = nil
	claim.CreationTimestamp = created
	claim.Spec.ServingPort = port
	claim.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
	require.NoError(t, r.Create(context.Background(), claim))
	return getModel(t, r, name)
}

// warm-1 has room, but its resident a already serves on 9001, the port the
// newcomer asks for. warm-2 is full of idle models, and its pool keeps no wake
// reserve, so a sleep there would make room. Room should be made on warm-2.
func TestReviewAPortConflictElsewhereStillMakesRoom(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 2,
		serving{"a", "warm-1", 300, 100, at0758},
		serving{"b", "warm-2", 300, 100, at0758},
		serving{"c", "warm-2", 300, 100, at0758},
		serving{"d", "warm-2", 300, 100, at0759},
	)
	claim := reviewPortClaim(t, r, "newcomer", ptr.To[int32](9001), metav1.NewTime(at0759))

	reconcileOnce(t, r, claim.Name)

	condition := scheduledOf(t, r, claim.Name)
	t.Logf("Scheduled %s: %s; sleeps %v; activates %d", condition.Reason, condition.Message, runtime.sleepCalls, len(runtime.activateCalls))
	assert.NotEmpty(t, runtime.sleepCalls, "an idle model on warm-2 is put to sleep to make room")
	assert.Equal(t, scheduledReasonMakingRoom, condition.Reason)
}

// warm-1 is full of idle models, and a serves on 9001, the port the newcomer
// asks for. A sleeping engine keeps its port, so no sleep there can let the
// newcomer in, and none should be asked for.
func TestReviewNoRoomIsMadeWhereThePortIsTaken(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758},
		serving{"b", "warm-1", 300, 100, at0758},
		serving{"c", "warm-1", 300, 100, at0759},
	)
	claim := reviewPortClaim(t, r, "newcomer", ptr.To[int32](9001), metav1.NewTime(at0759))

	reconcileOnce(t, r, claim.Name)

	condition := scheduledOf(t, r, claim.Name)
	t.Logf("Scheduled %s: %s; sleeps %v", condition.Reason, condition.Message, runtime.sleepCalls)
	assert.Empty(t, runtime.sleepCalls, "no model is put to sleep on a Pod whose port stays taken")
	assert.NotEqual(t, scheduledReasonMakingRoom, condition.Reason)

	// The next pass: whatever was put to sleep, the port is still a's.
	reconcileOnce(t, r, claim.Name)
	condition = scheduledOf(t, r, claim.Name)
	ip := podNamed(t, r, "warm-1").Status.PodIP
	for _, m := range runtime.snapshots[ip].Models {
		t.Logf("after the second pass: %s phase=%s port=%d", m.ModelName, m.Phase, m.Port)
	}
	t.Logf("second pass: Scheduled %s: %s; instances %d; activates %d", condition.Reason, condition.Message,
		len(getModel(t, r, claim.Name).Status.Instances), len(runtime.activateCalls))
	for pass := 3; pass <= 4; pass++ {
		reconcileOnce(t, r, claim.Name)
		condition = scheduledOf(t, r, claim.Name)
		t.Logf("pass %d: Scheduled %s: %s; sleeps so far %d; activates %d", pass, condition.Reason, condition.Message,
			len(runtime.sleepCalls), len(runtime.activateCalls))
		assert.Empty(t, runtime.sleepCalls, "port-blocked Pod stays untouched across passes")
		assert.Equal(t, "PortInUse", condition.Reason)
	}
}

// The older claim asks for 9001, which a holds on the only Pod. It cannot be
// placed, so it should not hold up the newer claim, which asks for no port.
func TestReviewAClaimWhosePortIsTakenKeepsNoTurn(t *testing.T) {
	r, _ := servingPool(t, keepNoWakeReserve, 1, serving{"a", "warm-1", 300, 100, at0758})
	reviewPortClaim(t, r, "older", ptr.To[int32](9001), metav1.NewTime(at0758))
	newer := reviewPortClaim(t, r, "newer", nil, metav1.NewTime(at0759))

	reconcileOnce(t, r, newer.Name)

	condition := scheduledOf(t, r, newer.Name)
	t.Logf("newer: Scheduled %s: %s; instances %d", condition.Reason, condition.Message, len(getModel(t, r, newer.Name).Status.Instances))
	assert.Len(t, getModel(t, r, newer.Name).Status.Instances, 1, "the newer claim is placed")
}
