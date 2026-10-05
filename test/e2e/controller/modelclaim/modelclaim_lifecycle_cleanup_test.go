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

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestWaitForLifecyclePoolPodsDeleted(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	listCalls := 0
	selector := ""
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		selector = action.(k8stesting.ListAction).GetListRestrictions().Labels.String()
		if listCalls < 3 {
			return true, &corev1.PodList{Items: []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app": lifecycleDeploymentName,
				}},
			}}}, nil
		}
		return true, &corev1.PodList{}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitForLifecyclePoolPodsDeleted(ctx, client, time.Millisecond)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, listCalls, 3)
	assert.Equal(t, "app="+lifecycleDeploymentName, selector)
}

func TestLifecyclePoolDeploymentTerminatesPromptly(t *testing.T) {
	deployment := lifecyclePoolDeployment()
	gracePeriod := deployment.Spec.Template.Spec.TerminationGracePeriodSeconds

	require.NotNil(t, gracePeriod)
	assert.LessOrEqual(t, *gracePeriod, int64(1))
}
