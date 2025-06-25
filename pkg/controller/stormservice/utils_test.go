/*
Copyright 2025 The Aibrix Team.

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

package stormservice

import (
	"testing"

	"github.com/stretchr/testify/assert"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
)

func newStormService(replicas int32, surge, unavail string) orchestrationv1alpha1.StormService {
	s := intstrutil.FromString(surge)
	u := intstrutil.FromString(unavail)
	return orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type:           orchestrationv1alpha1.RollingUpdateStormServiceStrategyType,
				MaxSurge:       &s,
				MaxUnavailable: &u,
			},
		},
	}
}

func TestMaxSurge_MaxUnavailable_MinAvailable_StormService(t *testing.T) {
	ss := newStormService(10, "25%", "20%") // surge=2, unavail=2
	assert.Equal(t, int32(3), MaxSurge(&ss))
	assert.Equal(t, int32(2), MaxUnavailable(ss))
	assert.Equal(t, int32(8), MinAvailable(&ss))
}

func TestSetAndRemoveStormServiceCondition(t *testing.T) {
	now := metav1.Now()
	status := &orchestrationv1alpha1.StormServiceStatus{}
	cond := orchestrationv1alpha1.Condition{
		Type:               orchestrationv1alpha1.StormServiceReady,
		Status:             corev1.ConditionFalse,
		Reason:             "Initializing",
		LastTransitionTime: &now,
	}

	SetStormServiceCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	// identical updates ignored
	SetStormServiceCondition(status, cond)
	assert.Len(t, status.Conditions, 1)

	RemoveStormServiceCondition(status, orchestrationv1alpha1.StormServiceReady)
	assert.Empty(t, status.Conditions)
}

func TestIsRollingUpdate(t *testing.T) {
	replicas := int32(1)
	ss := orchestrationv1alpha1.StormService{
		Spec: orchestrationv1alpha1.StormServiceSpec{Replicas: &replicas},
	}
	assert.True(t, IsRollingUpdate(&ss)) // default empty string treated as rolling

	ss.Spec.UpdateStrategy.Type = orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType
	assert.False(t, IsRollingUpdate(&ss))
}
