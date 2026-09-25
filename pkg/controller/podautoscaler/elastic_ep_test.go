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

package podautoscaler

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
)

// elasticEPFakeEngine is an HTTP server that answers elastic EP scaling probes.
type elasticEPFakeEngine struct {
	mu         sync.Mutex
	statusCode int
	body       string
	delay      time.Duration
	requests   int
	methods    []string
	paths      []string
}

func (e *elasticEPFakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.requests++
	e.methods = append(e.methods, r.Method)
	e.paths = append(e.paths, r.URL.Path)
	delay := e.delay
	e.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	w.WriteHeader(e.statusCode)
	_, _ = w.Write([]byte(e.body))
}

func (e *elasticEPFakeEngine) snapshot() (int, []string, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests, append([]string(nil), e.methods...), append([]string(nil), e.paths...)
}

func newElasticEPFakeEngine(t *testing.T, statusCode int, body string) (*httptest.Server, *elasticEPFakeEngine) {
	t.Helper()
	engine := &elasticEPFakeEngine{statusCode: statusCode, body: body}
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	return server, engine
}

func elasticEPPortOf(t *testing.T, server *httptest.Server) int32 {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return int32(port)
}

func elasticEPFreePort(t *testing.T) int32 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close free port listener: %v", err)
	}
	return int32(port)
}

func elasticEPTestPod(name, podIP string, ready bool, container corev1.Container) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{"app": "foo"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{container}},
		Status: corev1.PodStatus{PodIP: podIP},
	}
	condition := corev1.ConditionFalse
	if ready {
		condition = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: condition}}
	return pod
}

func elasticEPTestEngineContainer(args ...string) corev1.Container {
	return corev1.Container{Name: "vllm", Command: []string{"vllm", "serve"}, Args: args}
}

func TestElasticEPEngineContainer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		containers []corev1.Container
		wantFound  bool
		wantName   string
	}{
		{
			name:       "enable flag in args",
			containers: []corev1.Container{{Name: "vllm", Args: []string{"--enable-elastic-ep"}}},
			wantFound:  true,
			wantName:   "vllm",
		},
		{
			name:       "enable flag in command",
			containers: []corev1.Container{{Name: "vllm", Command: []string{"vllm", "serve", "--enable-elastic-ep"}}},
			wantFound:  true,
			wantName:   "vllm",
		},
		{
			name:       "max dp size flag with value",
			containers: []corev1.Container{{Name: "vllm", Args: []string{"--elastic-ep-max-dp-size", "8"}}},
			wantFound:  true,
			wantName:   "vllm",
		},
		{
			name:       "max dp size flag inline",
			containers: []corev1.Container{{Name: "vllm", Args: []string{"--elastic-ep-max-dp-size=8"}}},
			wantFound:  true,
			wantName:   "vllm",
		},
		{
			name:       "unrelated flags",
			containers: []corev1.Container{{Name: "vllm", Args: []string{"--model", "m", "--tensor-parallel-size", "2"}}},
		},
		{
			name: "sidecar without flag and engine with flag",
			containers: []corev1.Container{
				{Name: "sidecar", Args: []string{"--metrics"}},
				{Name: "vllm", Args: []string{"--enable-elastic-ep"}},
			},
			wantFound: true,
			wantName:  "vllm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: tt.containers}}
			container, found := elasticEPEngineContainer(pod)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && container.Name != tt.wantName {
				t.Fatalf("container = %q, want %q", container.Name, tt.wantName)
			}
		})
	}
}

func TestElasticEPEnginePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		labels    map[string]string
		container corev1.Container
		want      int32
		wantOK    bool
	}{
		{
			name:      "label with data parallel size",
			labels:    map[string]string{constants.ModelLabelPort: "8000"},
			container: corev1.Container{Env: []corev1.EnvVar{{Name: "data-parallel-size", Value: "3"}}},
			want:      8000,
			wantOK:    true,
		},
		{
			name:      "label without data parallel size",
			labels:    map[string]string{constants.ModelLabelPort: "8000"},
			container: corev1.Container{},
			want:      8000,
			wantOK:    true,
		},
		{
			name:      "port argument",
			container: corev1.Container{Args: []string{"--port", "9000"}},
			want:      9000,
			wantOK:    true,
		},
		{
			name:      "inline port argument",
			container: corev1.Container{Args: []string{"--port=9001"}},
			want:      9001,
			wantOK:    true,
		},
		{
			name:      "vllm port env",
			container: corev1.Container{Env: []corev1.EnvVar{{Name: "VLLM_PORT", Value: "9002"}}},
			want:      9002,
			wantOK:    true,
		},
		{
			name:      "container port",
			container: corev1.Container{Ports: []corev1.ContainerPort{{ContainerPort: 9003, Protocol: corev1.ProtocolTCP}}},
			want:      9003,
			wantOK:    true,
		},
		{
			name:      "label wins over the command line",
			labels:    map[string]string{constants.ModelLabelPort: "8000"},
			container: corev1.Container{Args: []string{"--port", "9000"}},
			want:      8000,
			wantOK:    true,
		},
		{
			name:      "no port information",
			container: corev1.Container{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Labels: tt.labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{tt.container}},
			}
			got, ok := elasticEPEnginePort(pod, &pod.Spec.Containers[0])
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("port = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestElasticEPProber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		body       string
		delay      time.Duration
		timeout    time.Duration
		want       elasticEPProbeOutcome
		// skipRequestCheck drops the server side request assertions for cases
		// where the client can time out before the request is delivered.
		skipRequestCheck bool
	}{
		{name: "idle", statusCode: http.StatusOK, body: `{"is_scaling_elastic_ep": false}`, want: elasticEPProbeIdle},
		{name: "scaling flag", statusCode: http.StatusOK, body: `{"is_scaling_elastic_ep": true}`, want: elasticEPProbeScaling},
		{
			name:       "scaling commit",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error": "The model is currently scaling. Please try again later."}`,
			want:       elasticEPProbeScaling,
		},
		{
			name:       "busy marker in a different status",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error": "The model is currently scaling. Please try again later."}`,
			want:       elasticEPProbeUnavailable,
		},
		{name: "unrelated 503", statusCode: http.StatusServiceUnavailable, body: `{"error": "not ready"}`, want: elasticEPProbeUnavailable},
		{name: "unsupported endpoint", statusCode: http.StatusNotFound, body: `404 page not found`, want: elasticEPProbeUnavailable},
		{name: "server error", statusCode: http.StatusInternalServerError, body: `boom`, want: elasticEPProbeUnavailable},
		{name: "invalid payload", statusCode: http.StatusOK, body: `not-json`, want: elasticEPProbeUnavailable},
		{name: "empty payload", statusCode: http.StatusOK, body: `{}`, want: elasticEPProbeIdle},
		{
			name:             "request timeout",
			statusCode:       http.StatusOK,
			body:             `{"is_scaling_elastic_ep": false}`,
			delay:            200 * time.Millisecond,
			timeout:          20 * time.Millisecond,
			want:             elasticEPProbeUnavailable,
			skipRequestCheck: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			engine := &elasticEPFakeEngine{statusCode: tt.statusCode, body: tt.body, delay: tt.delay}
			server := httptest.NewServer(engine)
			t.Cleanup(server.Close)

			prober := newElasticEPProber()
			if tt.timeout > 0 {
				prober = &elasticEPProber{client: &http.Client{Timeout: tt.timeout}}
			}

			outcome, err := prober.probe(context.Background(), "127.0.0.1", elasticEPPortOf(t, server))
			if outcome != tt.want {
				t.Fatalf("outcome = %v (err=%v), want %v", outcome, err, tt.want)
			}

			if !tt.skipRequestCheck {
				requests, methods, paths := engine.snapshot()
				if requests == 0 {
					t.Fatal("expected at least one probe request")
				}
				if methods[0] != http.MethodPost {
					t.Fatalf("method = %q, want %q", methods[0], http.MethodPost)
				}
				if paths[0] != elasticEPProbePath {
					t.Fatalf("path = %q, want %q", paths[0], elasticEPProbePath)
				}
			}
		})
	}
}

// Bodies the fake engines answer elastic EP scaling probes with.
const (
	elasticEPIdleBody   = `{"is_scaling_elastic_ep": false}`
	elasticEPBusyBody   = `{"is_scaling_elastic_ep": true}`
	elasticEPCommitBody = `{"error": "The model is currently scaling. Please try again later."}`
)

// elasticEPEnginePod builds a target pod that runs one fake engine answering
// scaling state probes with the given response.
func elasticEPEnginePod(t *testing.T, name string, ready bool, statusCode int, body string) (*corev1.Pod, *elasticEPFakeEngine) {
	t.Helper()
	server, engine := newElasticEPFakeEngine(t, statusCode, body)
	pod := elasticEPTestPod(name, "127.0.0.1", ready, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPPortOf(t, server)))))
	return pod, engine
}

// elasticEPDisabledPod builds a target pod whose engine does not enable
// elastic EP.
func elasticEPDisabledPod(t *testing.T, name string) (*corev1.Pod, *elasticEPFakeEngine) {
	t.Helper()
	server, engine := newElasticEPFakeEngine(t, http.StatusOK, elasticEPIdleBody)
	pod := elasticEPTestPod(name, "127.0.0.1", true, elasticEPTestEngineContainer(
		"--port", strconv.Itoa(int(elasticEPPortOf(t, server)))))
	return pod, engine
}

// elasticEPUnreachablePod builds an engine pod whose resolved port accepts no
// connections.
func elasticEPUnreachablePod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	return elasticEPTestPod(name, "127.0.0.1", true, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPFreePort(t)))))
}

func TestObserveElasticEPScaling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &PodAutoscalerReconciler{elasticEPProber: newElasticEPProber()}

	t.Run("every engine answers", func(t *testing.T) {
		scalingPod, scalingEngine := elasticEPEnginePod(t, "pod-scaling", true, http.StatusOK, elasticEPBusyBody)
		idlePod, idleEngine := elasticEPEnginePod(t, "pod-idle", true, http.StatusOK, elasticEPIdleBody)
		notReadyPod, commitEngine := elasticEPEnginePod(t, "pod-not-ready", false, http.StatusServiceUnavailable, elasticEPCommitBody)
		plainPod, plainEngine := elasticEPDisabledPod(t, "pod-without-flag")

		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*scalingPod, *idlePod, *notReadyPod, *plainPod})
		if observation.state != elasticEPScalingComplete {
			t.Fatalf("state = %v, want complete", observation.state)
		}
		status := observation.status
		if status == nil {
			t.Fatal("expected an observed status")
		}
		if !status.InProgress {
			t.Fatal("InProgress = false, want true")
		}
		if status.ObservedEngines != 3 || status.ScalingEngines != 2 {
			t.Fatalf("counts = %d/%d, want 3/2", status.ObservedEngines, status.ScalingEngines)
		}
		for name, engine := range map[string]*elasticEPFakeEngine{
			"scaling":   scalingEngine,
			"idle":      idleEngine,
			"not ready": commitEngine,
		} {
			if requests, _, _ := engine.snapshot(); requests != 1 {
				t.Fatalf("%s engine requests = %d, want 1", name, requests)
			}
		}
		if requests, _, _ := plainEngine.snapshot(); requests != 0 {
			t.Fatalf("pod without the elastic EP flag was probed %d times, want 0", requests)
		}
	})

	t.Run("no elastic EP pods", func(t *testing.T) {
		plainPod, _ := elasticEPDisabledPod(t, "pod-without-flag")
		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*plainPod})
		if observation.state != elasticEPScalingAbsent {
			t.Fatalf("state = %v, want absent", observation.state)
		}
		if observation.status != nil {
			t.Fatalf("status = %+v, want nil", observation.status)
		}
	})

	t.Run("engine port cannot be resolved", func(t *testing.T) {
		pod := elasticEPTestPod("pod-without-port", "127.0.0.1", true, elasticEPTestEngineContainer("--enable-elastic-ep"))
		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*pod})
		if observation.state != elasticEPScalingIncomplete {
			t.Fatalf("state = %v, want incomplete", observation.state)
		}
	})

	t.Run("engine pod has no address yet", func(t *testing.T) {
		pod := elasticEPTestPod("pod-pending", "", true, elasticEPTestEngineContainer(
			"--enable-elastic-ep", "--port", "8000"))
		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*pod})
		if observation.state != elasticEPScalingIncomplete {
			t.Fatalf("state = %v, want incomplete", observation.state)
		}
	})

	t.Run("one of two engines does not answer", func(t *testing.T) {
		idlePod, _ := elasticEPEnginePod(t, "pod-idle", true, http.StatusOK, elasticEPIdleBody)
		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*idlePod, *elasticEPUnreachablePod(t, "pod-dead")})
		if observation.state != elasticEPScalingIncomplete {
			t.Fatalf("state = %v, want incomplete", observation.state)
		}
		if observation.status != nil {
			t.Fatalf("status = %+v, want nil for a partial pass", observation.status)
		}
	})

	t.Run("no engine answers", func(t *testing.T) {
		observation := r.observeElasticEPScaling(ctx, []corev1.Pod{*elasticEPUnreachablePod(t, "pod-dead")})
		if observation.state != elasticEPScalingIncomplete {
			t.Fatalf("state = %v, want incomplete", observation.state)
		}
	})
}

func TestMergeElasticEPScalingStatus(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())
	earlier := metav1.NewTime(now.Add(-time.Minute))
	status := func(inProgress bool, transitionTime *metav1.Time) *autoscalingv1alpha1.ElasticEPScalingStatus {
		return &autoscalingv1alpha1.ElasticEPScalingStatus{
			InProgress:         inProgress,
			ObservedEngines:    2,
			ScalingEngines:     1,
			LastTransitionTime: transitionTime,
		}
	}

	tests := []struct {
		name     string
		previous *autoscalingv1alpha1.ElasticEPScalingStatus
		observed *autoscalingv1alpha1.ElasticEPScalingStatus
		wantTime time.Time
	}{
		{
			name:     "first observation",
			observed: status(false, nil),
			wantTime: now.Time,
		},
		{
			name:     "same state keeps the transition time",
			previous: status(false, &earlier),
			observed: status(false, nil),
			wantTime: earlier.Time,
		},
		{
			name:     "state change moves the transition time",
			previous: status(true, &earlier),
			observed: status(false, nil),
			wantTime: now.Time,
		},
		{
			name:     "missing transition time is refilled",
			previous: status(false, nil),
			observed: status(false, nil),
			wantTime: now.Time,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			merged := mergeElasticEPScalingStatus(tt.previous, tt.observed, now)
			if merged.InProgress != tt.observed.InProgress {
				t.Fatalf("InProgress = %v, want %v", merged.InProgress, tt.observed.InProgress)
			}
			if merged.ObservedEngines != tt.observed.ObservedEngines || merged.ScalingEngines != tt.observed.ScalingEngines {
				t.Fatalf("counts = %d/%d, want %d/%d", merged.ObservedEngines, merged.ScalingEngines,
					tt.observed.ObservedEngines, tt.observed.ScalingEngines)
			}
			if merged.LastTransitionTime == nil || !merged.LastTransitionTime.Time.Equal(tt.wantTime) {
				t.Fatalf("LastTransitionTime = %v, want %v", merged.LastTransitionTime, tt.wantTime)
			}
		})
	}
}

func TestStatusConstructorsPreserveElasticEPScaling(t *testing.T) {
	t.Parallel()

	existing := &autoscalingv1alpha1.ElasticEPScalingStatus{
		InProgress:      true,
		ObservedEngines: 3,
		ScalingEngines:  1,
	}

	t.Run("setStatus", func(t *testing.T) {
		t.Parallel()
		pa := &autoscalingv1alpha1.PodAutoscaler{}
		pa.Status.ElasticEPScaling = existing
		setStatus(pa, 1, 2, true, "test", false, true, nil)
		if pa.Status.ElasticEPScaling == nil || !pa.Status.ElasticEPScaling.InProgress {
			t.Fatalf("ElasticEPScaling was not preserved: %+v", pa.Status.ElasticEPScaling)
		}
	})

	t.Run("computeStatus", func(t *testing.T) {
		t.Parallel()
		pa := autoscalingv1alpha1.PodAutoscaler{}
		pa.Status.ElasticEPScaling = existing
		status := computeStatus(context.Background(), pa, ValidationResult{Valid: true}, ValidationResult{Valid: true})
		if status.ElasticEPScaling == nil || status.ElasticEPScaling.ObservedEngines != 3 {
			t.Fatalf("ElasticEPScaling was not preserved: %+v", status.ElasticEPScaling)
		}
	})
}

func TestMergeElasticEPScalingObservation(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())
	earlier := metav1.NewTime(now.Add(-time.Minute))
	previous := &autoscalingv1alpha1.ElasticEPScalingStatus{
		InProgress:         true,
		ObservedEngines:    2,
		ScalingEngines:     1,
		LastTransitionTime: &earlier,
	}
	fresh := &autoscalingv1alpha1.ElasticEPScalingStatus{
		InProgress:      true,
		ObservedEngines: 2,
		ScalingEngines:  1,
	}

	t.Run("absent observation clears the status", func(t *testing.T) {
		t.Parallel()
		if merged := mergeElasticEPScalingObservation(previous, elasticEPScalingObservation{state: elasticEPScalingAbsent}, now); merged != nil {
			t.Fatalf("status = %+v, want nil", merged)
		}
		if merged := mergeElasticEPScalingObservation(nil, elasticEPScalingObservation{state: elasticEPScalingAbsent}, now); merged != nil {
			t.Fatalf("status = %+v, want nil", merged)
		}
	})

	t.Run("incomplete observation keeps the previous status", func(t *testing.T) {
		t.Parallel()
		merged := mergeElasticEPScalingObservation(previous, elasticEPScalingObservation{state: elasticEPScalingIncomplete}, now)
		if merged != previous {
			t.Fatalf("status = %+v, want the previous status", merged)
		}
		if merged.LastTransitionTime == nil || !merged.LastTransitionTime.Time.Equal(earlier.Time) {
			t.Fatalf("LastTransitionTime = %v, want %v", merged.LastTransitionTime, earlier.Time)
		}
	})

	t.Run("incomplete observation keeps an empty status empty", func(t *testing.T) {
		t.Parallel()
		if merged := mergeElasticEPScalingObservation(nil, elasticEPScalingObservation{state: elasticEPScalingIncomplete}, now); merged != nil {
			t.Fatalf("status = %+v, want nil", merged)
		}
	})

	t.Run("complete observation merges the counts", func(t *testing.T) {
		t.Parallel()
		merged := mergeElasticEPScalingObservation(previous, elasticEPScalingObservation{state: elasticEPScalingComplete, status: fresh}, now)
		if merged == nil {
			t.Fatal("expected a merged status")
		}
		if merged.ObservedEngines != 2 || merged.ScalingEngines != 1 {
			t.Fatalf("counts = %d/%d, want 2/1", merged.ObservedEngines, merged.ScalingEngines)
		}
		if merged.LastTransitionTime == nil || !merged.LastTransitionTime.Time.Equal(earlier.Time) {
			t.Fatalf("LastTransitionTime = %v, want %v", merged.LastTransitionTime, earlier.Time)
		}
	})
}

func TestReconcileCustomPAWritesElasticEPScaling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	server, engine := newElasticEPFakeEngine(t, http.StatusServiceUnavailable, elasticEPCommitBody)
	notReadyPod := elasticEPTestPod("pod-scaling", "127.0.0.1", false, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPPortOf(t, server)))))

	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pa-elastic-ep"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  ns,
				Name:       "test-deployment",
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "50",
			}},
		},
	}
	scaleTarget := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "test-deployment",
			"namespace": ns,
		},
		"spec": map[string]interface{}{"replicas": int64(1)},
	}}

	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(pa, notReadyPod, scaleTarget).WithStatusSubresource(pa).Build()

	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, apimeta.RESTScopeNamespace)

	r := &PodAutoscalerReconciler{
		Client:              cl,
		Scheme:              sch,
		EventRecorder:       record.NewFakeRecorder(16),
		Mapper:              mapper,
		workloadScaleClient: &fakeWorkloadScaleClient{},
		autoScaler:          &fakeAutoScaler{},
		monitor:             monitor.New(),
		elasticEPProber:     newElasticEPProber(),
	}

	if _, err := r.reconcileCustomPA(ctx, *pa); err != nil {
		t.Fatalf("reconcileCustomPA returned error: %v", err)
	}

	stored := &autoscalingv1alpha1.PodAutoscaler{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(pa), stored); err != nil {
		t.Fatalf("get PodAutoscaler: %v", err)
	}
	status := stored.Status.ElasticEPScaling
	if status == nil {
		t.Fatal("expected status.elasticEPScaling to be written")
	}
	if !status.InProgress {
		t.Fatal("InProgress = false, want true")
	}
	if status.ObservedEngines != 1 || status.ScalingEngines != 1 {
		t.Fatalf("counts = %d/%d, want 1/1", status.ObservedEngines, status.ScalingEngines)
	}
	if status.LastTransitionTime == nil {
		t.Fatal("expected a transition time")
	}
	if requests, _, _ := engine.snapshot(); requests != 1 {
		t.Fatalf("engine requests = %d, want 1", requests)
	}
}
