package cache

import (
    "testing"

    "github.com/stretchr/testify/require"
    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

    "github.com/vllm-project/aibrix/pkg/constants"
)

func TestPodMetricTargetsModelClaims(t *testing.T) {
    pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
        Name: "warm",
        Annotations: map[string]string{
            constants.ModelClaimPodAnnotationPrefix + "qwen":  `{"model":"qwen","port":20001,"state":"active"}`,
            constants.ModelClaimPodAnnotationPrefix + "llama": `{"model":"llama","port":20000,"state":"active"}`,
            constants.ModelClaimPodAnnotationPrefix + "sleep": `{"model":"sleep","port":0,"state":"sleeping"}`,
        },
    }}}
    require.Equal(t, []podMetricTarget{
        {model: "llama", port: 20000},
        {model: "qwen", port: 20001},
    }, podMetricTargets(pod))
}

func TestPodMetricTargetsSleepingClaimsHaveNoFallback(t *testing.T) {
    pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
        Name: "warm",
        Annotations: map[string]string{
            constants.ModelClaimPodAnnotationPrefix + "sleep": `{"model":"sleep","port":0,"state":"sleeping"}`,
        },
    }}}
    require.Empty(t, podMetricTargets(pod))
}

func TestPodMetricTargetsOrdinaryPodRetainsLegacyPort(t *testing.T) {
    pod := &Pod{Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
        Name: "regular",
        Labels: map[string]string{MetricPortLabel: "9999"},
    }}}
    require.Equal(t, []podMetricTarget{{port: 9999}}, podMetricTargets(pod))
}
