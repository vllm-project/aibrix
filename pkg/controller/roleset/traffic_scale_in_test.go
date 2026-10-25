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

package roleset

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/internal/trafficdraintest"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/trafficdrain"
)

// Literal annotation names let the same acceptance tests run on the old commit.
const trafficWaitKey = "aibrix.ai/traffic-scale-in-max-wait"
const trafficRetryKey = "aibrix.ai/traffic-scale-in-retry-at"

func trafficGauges(running, waiting string) string {
	return "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"example/model\"} " + running +
		"\n# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting{model_name=\"example/model\"} " + waiting + "\n"
}

func trafficTestPod(t *testing.T, name string, body *atomic.Value, code int) *v1.Pod {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	pod := newTestPod(name, "test-ns", "worker", "test-roleset", true, false)
	pod.UID = types.UID(name + "-uid")
	pod.Status.PodIP = host
	pod.Labels[aibrixconst.ModelLabelMetricPort] = port
	return pod
}

func trafficFixture(t *testing.T, pods ...*v1.Pod) (*StatelessRoleSyncer, client.Client, *orchestrationv1alpha1.RoleSet, *orchestrationv1alpha1.RoleSpec) {
	a, b, c, d, _ := trafficFixtureBackend(t, pods...)
	return a, b, c, d
}
func trafficFixtureBackend(t *testing.T, pods ...*v1.Pod) (*StatelessRoleSyncer, client.Client, *orchestrationv1alpha1.RoleSet, *orchestrationv1alpha1.RoleSpec, *miniredis.Miniredis) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, orchestrationv1alpha1.AddToScheme(scheme))
	owner := newTestRoleSet("test-roleset", "test-ns")
	role := newTestRoleSpec("worker", int32(len(pods)-1), intStrPtr(intstr.FromInt(1)), intStrPtr(intstr.FromInt(1)))
	role.Drain = &orchestrationv1alpha1.RoleDrainSpec{TimeoutSeconds: ptr.To[int32](1)}
	role.Template.Annotations = map[string]string{trafficWaitKey: "30s"}
	mr, redisClient, _ := trafficdraintest.NewRedis(t)
	gate := trafficdrain.New(redisClient)
	t.Cleanup(func() { _ = gate.Close() })
	now := time.Now()
	mr.SetTime(now)
	for _, pod := range pods {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[aibrixconst.TrafficScaleInMaxWaitAnnotationKey] = "30s"
		pod.Annotations[aibrixconst.TrafficDrainProtocolAnnotationKey] = aibrixconst.TrafficDrainProtocolVersion
		epoch, err := gate.Initialize(context.Background(), string(pod.UID))
		require.NoError(t, err)
		pod.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey] = epoch
		if pod.Annotations[aibrixconst.PodDrainingAnnotationKey] == "true" {
			start, err := time.Parse(time.RFC3339, pod.Annotations[aibrixconst.PodDrainStartTimeAnnotationKey])
			require.NoError(t, err)
			mr.SetTime(start)
			_, err = gate.Start(context.Background(), string(pod.UID), epoch, 30*time.Second)
			require.NoError(t, err)
			mr.SetTime(now)
		}
	}
	objects := []client.Object{owner}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &StatelessRoleSyncer{cli: cli, computeHashFunc: fakeComputeHashFunc, gate: gate}, cli, owner, role, mr
}

func trafficMarkDraining(pod *v1.Pod, ago time.Duration) {
	pod.Annotations = map[string]string{
		aibrixconst.PodDrainingAnnotationKey:          "true",
		aibrixconst.PodDrainStartTimeAnnotationKey:    time.Now().Add(-ago).UTC().Format(time.RFC3339),
		aibrixconst.PodDrainReasonAnnotationKey:       aibrixconst.PodDrainReasonScaleIn,
		aibrixconst.PodDrainTargetActionAnnotationKey: aibrixconst.PodDrainTargetActionDelete,
	}
}

func TestTrafficAwareScaleInChoosesLowerLoad(t *testing.T) {
	var busy, idle atomic.Value
	busy.Store(trafficGauges("9", "2"))
	idle.Store(trafficGauges("0", "0"))
	newest := trafficTestPod(t, "newest-busy", &busy, 200)
	oldest := trafficTestPod(t, "oldest-idle", &idle, 200)
	newest.CreationTimestamp = metav1.NewTime(time.Now())
	oldest.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	syncer, cli, owner, role := trafficFixture(t, newest, oldest)
	for i := 0; i < 11; i++ {
		require.NoError(t, syncer.gate.Admit(context.Background(), string(newest.UID), newest.Annotations[aibrixconst.TrafficDrainEpochAnnotationKey], fmt.Sprint("busy-", i)))
	}
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	for _, pod := range []*v1.Pod{newest, oldest} {
		got := &v1.Pod{}
		require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
		require.Equal(t, pod == oldest, got.Annotations[aibrixconst.PodDrainingAnnotationKey] == "true", pod.Name)
	}
}

func TestTrafficAwareScaleInRetainsBusyAndUnknown(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
	}{
		{"running", trafficGauges("1", "0"), 200},
		{"waiting", trafficGauges("0", "1"), 200},
		{"missing", "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running{model_name=\"example/model\"} 0\n", 200},
		{"http-error", trafficGauges("0", "0"), 503},
		{"nan", trafficGauges("NaN", "0"), 200},
		{"negative", trafficGauges("-1", "0"), 200},
		{"infinite", trafficGauges("+Inf", "0"), 200},
		{"different-models", strings.Replace(trafficGauges("0", "0"), "waiting{model_name=\"example/model\"}", "waiting{model_name=\"another\"}", 1), 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(tc.body)
			pod := trafficTestPod(t, "draining", &body, tc.code)
			trafficMarkDraining(pod, 2*time.Second)
			syncer, cli, owner, role := trafficFixture(t, pod)
			result, err := syncer.Scale(context.Background(), owner, role)
			require.NoError(t, err)
			require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
			require.Positive(t, result.RequeueAfter)
		})
	}
}

func TestTrafficAwareScaleInCompletesAfterFreshIdleSample(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("1", "0"))
	pod := trafficTestPod(t, "draining", &body, 200)
	trafficMarkDraining(pod, 2*time.Second)
	syncer, cli, owner, role := trafficFixture(t, pod)
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
	body.Store(trafficGauges("0", "0"))
	_, err = syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{})))
}

func TestTrafficAwareScaleInTimeoutCancelsAndBacksOff(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("3", "0"))
	pod := trafficTestPod(t, "draining", &body, 200)
	trafficMarkDraining(pod, time.Minute)
	syncer, cli, owner, role := trafficFixture(t, pod)
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	got := &v1.Pod{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	require.Empty(t, got.Annotations[aibrixconst.PodDrainingAnnotationKey])
	retry, err := time.Parse(time.RFC3339Nano, got.Annotations[trafficRetryKey])
	require.NoError(t, err)
	require.True(t, retry.After(time.Now()))
	// A new syncer models a reconciler restart: cooldown lives on the Pod.
	restarted := &StatelessRoleSyncer{cli: cli, computeHashFunc: fakeComputeHashFunc, gate: syncer.gate}
	_, err = restarted.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	require.Empty(t, got.Annotations[aibrixconst.PodDrainingAnnotationKey])
}

func TestTrafficAwareScaleInHonorsMinimumWaitAndCancelsOnScaleUp(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	pod := trafficTestPod(t, "draining", &body, 200)
	trafficMarkDraining(pod, 0)
	syncer, cli, owner, role := trafficFixture(t, pod)
	role.Drain.TimeoutSeconds = ptr.To[int32](10)
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
	role.Replicas = ptr.To[int32](1)
	_, err = syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	got := &v1.Pod{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	require.Empty(t, got.Annotations[aibrixconst.PodDrainingAnnotationKey])
}

func TestTrafficAwareScaleInOptOutCannotRemovePublishedProtection(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("5", "0"))
	pod := trafficTestPod(t, "legacy", &body, 200)
	trafficMarkDraining(pod, 2*time.Second)
	syncer, cli, owner, role := trafficFixture(t, pod)
	delete(role.Template.Annotations, trafficWaitKey)
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
}

func TestTrafficAwareScaleInRejectsInvalidConfiguration(t *testing.T) {
	for _, mode := range []string{"empty", "malformed", "short", "no-minimum", "stateful", "podset"} {
		t.Run(mode, func(t *testing.T) {
			var body atomic.Value
			body.Store(trafficGauges("0", "0"))
			pod := trafficTestPod(t, "candidate", &body, 200)
			_, cli, owner, role := trafficFixture(t, pod)
			switch mode {
			case "empty":
				role.Template.Annotations[trafficWaitKey] = ""
			case "malformed":
				role.Template.Annotations[trafficWaitKey] = "soon"
			case "short":
				role.Template.Annotations[trafficWaitKey] = "1s"
			case "no-minimum":
				role.Drain = nil
			case "stateful":
				role.Stateful = true
			case "podset":
				role.PodGroupSize = ptr.To[int32](2)
			}
			owner.Spec.Roles = []orchestrationv1alpha1.RoleSpec{*role}
			r := &RoleSetReconciler{Client: cli}
			_, err := r.syncPods(context.Background(), owner)
			require.Error(t, err)
			require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
		})
	}
}

type trafficConcurrentChangeClient struct {
	client.Client
	replacement bool
}

func (c *trafficConcurrentChangeClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	current := &v1.Pod{}
	requireKey := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, requireKey, current); err != nil {
		return err
	}
	if c.replacement {
		if err := c.Client.Delete(ctx, current); err != nil {
			return err
		}
		current.UID = "replacement-uid"
		current.ResourceVersion = ""
		if err := c.Create(ctx, current); err != nil {
			return err
		}
	} else {
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["concurrent-change"] = "true"
		if err := c.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestTrafficAwareScaleInDeletionPreconditions(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint("replacement=", replacement), func(t *testing.T) {
			var body atomic.Value
			body.Store(trafficGauges("0", "0"))
			pod := trafficTestPod(t, "draining", &body, 200)
			trafficMarkDraining(pod, 2*time.Second)
			syncer, cli, owner, role := trafficFixture(t, pod)
			syncer.cli = &trafficConcurrentChangeClient{Client: cli, replacement: replacement}
			_, err := syncer.Scale(context.Background(), owner, role)
			require.Error(t, err)
			require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
		})
	}
}

func TestTrafficAwareScaleInKeepsAvailabilityFloor(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("0", "0"))
	ready := trafficTestPod(t, "ready", &body, 200)
	unready := trafficTestPod(t, "unready", &body, 200)
	unready.Status.Conditions[0].Status = v1.ConditionFalse
	syncer, cli, owner, role := trafficFixture(t, ready, unready)
	role.UpdateStrategy.MaxUnavailable = intStrPtr(intstr.FromInt(0))
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	for _, pod := range []*v1.Pod{ready, unready} {
		got := &v1.Pod{}
		require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
		require.Empty(t, got.Annotations[aibrixconst.PodDrainingAnnotationKey])
	}
}

func TestTrafficAwareScaleInAlsoProtectsManagedRollouts(t *testing.T) {
	var body atomic.Value
	body.Store(trafficGauges("5", "0"))
	pod := trafficTestPod(t, "rollout", &body, 200)
	trafficMarkDraining(pod, 2*time.Second)
	pod.Annotations[aibrixconst.PodDrainReasonAnnotationKey] = aibrixconst.PodDrainReasonRollout
	syncer, cli, owner, role := trafficFixture(t, pod)
	_, err := syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
}

func TestTrafficAwareScaleInWaitsForHeldResponse(t *testing.T) {
	var active atomic.Int32
	var once sync.Once
	release, finished := make(chan struct{}), make(chan struct{})
	unblock := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = fmt.Fprint(w, trafficGauges(fmt.Sprint(active.Load()), "0"))
			return
		}
		active.Add(1)
		defer func() { active.Add(-1); close(finished) }()
		_, _ = io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "completed\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { unblock(); srv.Close() })
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	pod := newTestPod("streaming", "test-ns", "worker", "test-roleset", true, false)
	pod.UID = "streaming-uid"
	pod.Status.PodIP = host
	pod.Labels[aibrixconst.ModelLabelMetricPort] = port
	trafficMarkDraining(pod, 2*time.Second)
	syncer, cli, owner, role := trafficFixture(t, pod)
	httpClient := srv.Client()
	httpClient.Timeout = 5 * time.Second
	response, err := httpClient.Get(srv.URL + "/request")
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.EqualValues(t, 1, active.Load())
	_, err = syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{}))
	unblock()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "completed")
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("response did not finish")
	}
	_, err = syncer.Scale(context.Background(), owner, role)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(cli.Get(context.Background(), client.ObjectKeyFromObject(pod), &v1.Pod{})))
}
