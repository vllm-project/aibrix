package routingalgorithms

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_PrefixCache(t *testing.T) {
	prefixCacheRouter, _ := NewPrefixCacheRouter()

	pods := map[string]*v1.Pod{
		"p1": {
			ObjectMeta: metav1.ObjectMeta{Name: "p1"},
			Status: v1.PodStatus{
				PodIP: "1.1.1.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
		"p2": {
			ObjectMeta: metav1.ObjectMeta{Name: "p2"},
			Status: v1.PodStatus{
				PodIP: "2.2.2.2",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			}},
	}

	targetPod, err := prefixCacheRouter.Route(context.Background(), pods, "m1", "this is first message")
	assert.NoError(t, err)

	targetPod2, err := prefixCacheRouter.Route(context.Background(), pods, "m1", "this is first message")
	assert.NoError(t, err)

	assert.Equal(t, targetPod, targetPod2)
}
