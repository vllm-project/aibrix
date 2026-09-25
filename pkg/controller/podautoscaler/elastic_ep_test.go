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
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
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

func TestElasticEPEnginePorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		labels    map[string]string
		container corev1.Container
		want      []int32
	}{
		{
			name:      "label with data parallel size",
			labels:    map[string]string{constants.ModelLabelPort: "8000"},
			container: corev1.Container{Env: []corev1.EnvVar{{Name: "data-parallel-size", Value: "3"}}},
			want:      []int32{8000, 8001, 8002},
		},
		{
			name:      "label without data parallel size",
			labels:    map[string]string{constants.ModelLabelPort: "8000"},
			container: corev1.Container{},
			want:      []int32{8000},
		},
		{
			name:      "port argument",
			container: corev1.Container{Args: []string{"--port", "9000"}},
			want:      []int32{9000},
		},
		{
			name:      "inline port argument",
			container: corev1.Container{Args: []string{"--port=9001"}},
			want:      []int32{9001},
		},
		{
			name:      "vllm port env",
			container: corev1.Container{Env: []corev1.EnvVar{{Name: "VLLM_PORT", Value: "9002"}}},
			want:      []int32{9002},
		},
		{
			name:      "container port",
			container: corev1.Container{Ports: []corev1.ContainerPort{{ContainerPort: 9003, Protocol: corev1.ProtocolTCP}}},
			want:      []int32{9003},
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
			got := elasticEPEnginePorts(pod, &pod.Spec.Containers[0])
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ports = %v, want %v", got, tt.want)
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

func TestProbeElasticEPTarget(t *testing.T) {
	t.Parallel()

	idleServer, _ := newElasticEPFakeEngine(t, http.StatusOK, `{"is_scaling_elastic_ep": false}`)
	scalingServer, _ := newElasticEPFakeEngine(t, http.StatusOK, `{"is_scaling_elastic_ep": true}`)
	prober := newElasticEPProber()

	tests := []struct {
		name         string
		ports        []int32
		wantObserved bool
		wantScaling  bool
	}{
		{
			name:         "idle port",
			ports:        []int32{elasticEPPortOf(t, idleServer)},
			wantObserved: true,
		},
		{
			name:         "scaling port",
			ports:        []int32{elasticEPPortOf(t, scalingServer)},
			wantObserved: true,
			wantScaling:  true,
		},
		{
			name:         "falls through an unreachable port",
			ports:        []int32{elasticEPFreePort(t), elasticEPPortOf(t, idleServer)},
			wantObserved: true,
		},
		{
			name:         "scaling port after an idle port",
			ports:        []int32{elasticEPPortOf(t, idleServer), elasticEPPortOf(t, scalingServer)},
			wantObserved: true,
			wantScaling:  true,
		},
		{
			name:  "all ports unreachable",
			ports: []int32{elasticEPFreePort(t)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := elasticEPProbeTarget{podName: "pod-a", ip: "127.0.0.1", ports: tt.ports}
			observed, scaling := probeElasticEPTarget(context.Background(), prober, target)
			if observed != tt.wantObserved {
				t.Fatalf("observed = %v, want %v", observed, tt.wantObserved)
			}
			if scaling != tt.wantScaling {
				t.Fatalf("scaling = %v, want %v", scaling, tt.wantScaling)
			}
		})
	}
}

func TestObserveElasticEPScaling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	scalingServer, scalingEngine := newElasticEPFakeEngine(t, http.StatusOK, `{"is_scaling_elastic_ep": true}`)
	idleServer, idleEngine := newElasticEPFakeEngine(t, http.StatusOK, `{"is_scaling_elastic_ep": false}`)
	notProbedServer, notProbedEngine := newElasticEPFakeEngine(t, http.StatusOK, `{"is_scaling_elastic_ep": false}`)

	podScaling := elasticEPTestPod("pod-scaling", "127.0.0.1", true, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPPortOf(t, scalingServer)))))
	podIdle := elasticEPTestPod("pod-idle", "127.0.0.1", true, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPPortOf(t, idleServer)))))
	podNotReady := elasticEPTestPod("pod-not-ready", "127.0.0.1", false, elasticEPTestEngineContainer(
		"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPPortOf(t, scalingServer)))))
	podWithoutFlag := elasticEPTestPod("pod-without-flag", "127.0.0.1", true, elasticEPTestEngineContainer(
		"--port", strconv.Itoa(int(elasticEPPortOf(t, notProbedServer)))))
	podWithoutPort := elasticEPTestPod("pod-without-port", "127.0.0.1", true, elasticEPTestEngineContainer(
		"--enable-elastic-ep"))

	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	cl := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(podScaling, podIdle, podNotReady, podWithoutFlag, podWithoutPort).
		Build()

	r := &PodAutoscalerReconciler{
		Client:              cl,
		workloadScaleClient: &fakeWorkloadScaleClient{},
	}
	pa := &autoscalingv1alpha1.PodAutoscaler{}
	pa.Namespace = ns
	scaleObj := buildScaleObject("apps/v1", "Deployment", ns, "foo-deploy")

	observed := r.observeElasticEPScaling(ctx, pa, scaleObj)
	if observed == nil {
		t.Fatal("expected an observation")
	}
	if !observed.InProgress {
		t.Fatalf("InProgress = false, want true")
	}
	if observed.ObservedEngines != 2 {
		t.Fatalf("ObservedEngines = %d, want 2", observed.ObservedEngines)
	}
	if observed.ScalingEngines != 1 {
		t.Fatalf("ScalingEngines = %d, want 1", observed.ScalingEngines)
	}
	if requests, _, _ := scalingEngine.snapshot(); requests != 1 {
		t.Fatalf("scaling engine requests = %d, want 1", requests)
	}
	if requests, _, _ := idleEngine.snapshot(); requests != 1 {
		t.Fatalf("idle engine requests = %d, want 1", requests)
	}
	if requests, _, _ := notProbedEngine.snapshot(); requests != 0 {
		t.Fatalf("pod without the elastic EP flag was probed %d times, want 0", requests)
	}

	t.Run("no engine answers", func(t *testing.T) {
		deadPod := elasticEPTestPod("pod-dead", "127.0.0.1", true, elasticEPTestEngineContainer(
			"--enable-elastic-ep", "--port", strconv.Itoa(int(elasticEPFreePort(t)))))
		deadCl := fake.NewClientBuilder().WithScheme(sch).WithObjects(deadPod).Build()
		deadReconciler := &PodAutoscalerReconciler{
			Client:              deadCl,
			workloadScaleClient: &fakeWorkloadScaleClient{},
		}
		if got := deadReconciler.observeElasticEPScaling(ctx, pa, scaleObj); got != nil {
			t.Fatalf("observation = %+v, want nil", got)
		}
	})

	t.Run("no elastic EP pods", func(t *testing.T) {
		plainCl := fake.NewClientBuilder().WithScheme(sch).WithObjects(podWithoutFlag).Build()
		plainReconciler := &PodAutoscalerReconciler{
			Client:              plainCl,
			workloadScaleClient: &fakeWorkloadScaleClient{},
		}
		if got := plainReconciler.observeElasticEPScaling(ctx, pa, scaleObj); got != nil {
			t.Fatalf("observation = %+v, want nil", got)
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
