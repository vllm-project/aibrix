/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

func TestExecuteExternalRequestLimitsAndCancellation(t *testing.T) {
	t.Run("request too large before exchange", func(t *testing.T) {
		cfg := externalRouterTestConfig("http://unused.invalid", PolicyAuthoritative, FailureFailClosed)
		cfg.maxRequestBytes = 1
		_, _, err := executeExternalRequest(context.Background(), http.DefaultClient, cfg, externalDecisionRequest{
			APIVersion: externalAPIVersion,
			Kind:       externalRequestKind,
			Metadata:   externalMetadata{RequestID: "req"},
		}, "")
		var classified *externalExchangeError
		require.ErrorAs(t, err, &classified)
		require.Equal(t, externalOutcomeRequestTooLarge, classified.outcome)
		require.False(t, classified.attempted)
	})

	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", externalMediaType)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 65))
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		cfg.maxResponseBytes = 64
		_, _, err := executeExternalRequest(context.Background(), server.Client(), cfg, externalDecisionRequest{
			APIVersion: externalAPIVersion,
			Kind:       externalRequestKind,
			Metadata:   externalMetadata{RequestID: "req"},
		}, "")
		var classified *externalExchangeError
		require.ErrorAs(t, err, &classified)
		require.Equal(t, externalOutcomeInvalidResponse, classified.outcome)
		require.True(t, classified.attempted)
	})

	t.Run("deadline", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		cfg.timeout = 5 * time.Millisecond
		_, _, err := executeExternalRequest(context.Background(), server.Client(), cfg, externalDecisionRequest{
			APIVersion: externalAPIVersion,
			Kind:       externalRequestKind,
			Metadata:   externalMetadata{RequestID: "req"},
		}, "")
		var classified *externalExchangeError
		require.ErrorAs(t, err, &classified)
		require.Equal(t, externalOutcomeTimeout, classified.outcome)
	})
}

type externalTestRouterFunc func(*types.RoutingContext, types.PodList) (string, error)

type externalPanickingDoer struct{}

func (externalPanickingDoer) Do(*http.Request) (*http.Response, error) {
	panic("external transport panic")
}

func (f externalTestRouterFunc) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	return f(ctx, pods)
}

func externalRouterTestConfig(endpoint string, policy externalPolicyMode, failure externalFailureMode) externalRouterConfig {
	parsed, _ := url.Parse(endpoint)
	return externalRouterConfig{
		endpoint:         parsed,
		policyMode:       policy,
		failureMode:      failure,
		fallback:         types.RoutingAlgorithm("least-request"),
		timeout:          time.Second,
		maxInflight:      2,
		maxRequestBytes:  256 * 1024,
		maxResponseBytes: 64 * 1024,
		failureThreshold: 2,
		openDuration:     time.Second,
		candidateMetrics: map[string]struct{}{},
	}
}

func externalResponseFor(requestID, decision, target string) string {
	targetJSON := ""
	if target != "" {
		targetJSON = fmt.Sprintf(",\"target\":{\"id\":%q,\"port\":8000}", target)
	}
	return fmt.Sprintf("{\"apiVersion\":\"routing.aibrix.ai/v1alpha1\",\"kind\":\"ReplicaSelectionResponse\",\"metadata\":{\"requestId\":%q},\"status\":{\"decision\":%q%s}}", requestID, decision, targetJSON)
}

func TestExternalRouterPolicies(t *testing.T) {
	podA := externalTestPod("default", "a", "10.0.0.1", "zone-a")
	podB := externalTestPod("default", "b", "10.0.0.2", "zone-b")
	pods := &utils.PodArray{Pods: []*v1.Pod{podA, podB}}

	t.Run("selected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			require.Equal(t, externalMediaType, req.Header.Get("Content-Type"))
			require.Equal(t, externalMediaType, req.Header.Get("Accept"))
			require.Equal(t, "req-1", req.Header.Get("X-Request-Id"))
			require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", req.Header.Get("traceparent"))
			w.Header().Set("Content-Type", externalMediaType)
			_, _ = w.Write([]byte(externalResponseFor("req-1", externalDecisionSelected, "default/b")))
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), nil, prometheus.NewRegistry())
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-1", "")
		ctx.ReqHeaders["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		address, err := router.Route(ctx, pods)
		require.NoError(t, err)
		require.Equal(t, "10.0.0.2:8000", address)
		require.Same(t, podB, ctx.TargetPod())
	})

	t.Run("advisory no decision uses fallback", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", externalMediaType)
			_, _ = w.Write([]byte(externalResponseFor("req-2", externalDecisionNoDecision, "")))
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAdvisory, FailureFailClosed)
		fallbackCalled := false
		selector := func(ctx *types.RoutingContext) (types.Router, error) {
			require.Equal(t, types.RoutingAlgorithm("least-request"), ctx.Algorithm)
			return externalTestRouterFunc(func(routeCtx *types.RoutingContext, candidates types.PodList) (string, error) {
				fallbackCalled = true
				routeCtx.SetTargetPod(candidates.All()[0])
				routeCtx.SetTargetPort(8000)
				return routeCtx.TargetAddress(), nil
			}), nil
		}
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), selector, prometheus.NewRegistry())
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-2", "")
		_, err := router.Route(ctx, pods)
		require.NoError(t, err)
		require.True(t, fallbackCalled)
		require.Equal(t, Algorithm, ctx.Algorithm)
	})

	t.Run("authoritative denied", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", externalMediaType)
			_, _ = w.Write([]byte(externalResponseFor("req-3", externalDecisionDenied, "")))
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), nil, prometheus.NewRegistry())
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-3", "")
		_, err := router.Route(ctx, pods)
		require.ErrorIs(t, err, ErrExternalPolicyDenied)
		require.False(t, ctx.HasRouted())
	})

	t.Run("fail closed on HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Error(w, "secret upstream details", http.StatusInternalServerError)
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), nil, prometheus.NewRegistry())
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-4", "")
		_, err := router.Route(ctx, pods)
		require.ErrorIs(t, err, ErrExternalRouterUnavailable)
		require.NotContains(t, err.Error(), "secret upstream details")
	})

	t.Run("fail open on HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAdvisory, FailureFailOpen)
		fallbackCalled := false
		selector := func(*types.RoutingContext) (types.Router, error) {
			return externalTestRouterFunc(func(routeCtx *types.RoutingContext, candidates types.PodList) (string, error) {
				fallbackCalled = true
				routeCtx.SetTargetPod(candidates.All()[0])
				routeCtx.SetTargetPort(8000)
				return routeCtx.TargetAddress(), nil
			}), nil
		}
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), selector, prometheus.NewRegistry())
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-5", "")
		address, err := router.Route(ctx, pods)
		require.NoError(t, err)
		require.True(t, fallbackCalled)
		require.Equal(t, "10.0.0.1:8000", address)
		require.Equal(t, Algorithm, ctx.Algorithm)
	})

	t.Run("failed fallback invocation is counted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		cfg := externalRouterTestConfig(server.URL, PolicyAdvisory, FailureFailOpen)
		selector := func(*types.RoutingContext) (types.Router, error) {
			return externalTestRouterFunc(func(*types.RoutingContext, types.PodList) (string, error) {
				return "", errors.New("local fallback failed")
			}), nil
		}
		registry := prometheus.NewRegistry()
		router := newExternalRouterWithDependencies(cfg, nil, server.Client(), selector, registry)
		ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "req-6", "")
		_, err := router.Route(ctx, pods)
		require.ErrorIs(t, err, ErrExternalRouterUnavailable)
		require.Equal(t, float64(1), testutil.ToFloat64(router.metrics.fallback.WithLabelValues(externalOutcomeHTTPError)))
	})
}

func TestExternalRouterSingleCandidateMode(t *testing.T) {
	cfg := externalRouterTestConfig("http://unused.invalid", PolicyAdvisory, FailureFailClosed)
	router := newExternalRouterWithDependencies(cfg, nil, nil, nil, prometheus.NewRegistry())
	require.True(t, router.BypassSingleCandidate())

	cfg.policyMode = PolicyAuthoritative
	authoritative := newExternalRouterWithDependencies(cfg, nil, nil, nil, prometheus.NewRegistry())
	require.False(t, authoritative.BypassSingleCandidate())
}

func TestExternalRoutersConcurrentlyShareDecisionService(t *testing.T) {
	const gatewayCount = 4

	podA := externalTestPod("default", "a", "10.0.0.1", "zone-a")
	podB := externalTestPod("default", "b", "10.0.0.2", "zone-b")
	pods := &utils.PodArray{Pods: []*v1.Pod{podA, podB}}
	expectedTargets := make(map[string]string, gatewayCount)
	for i := 0; i < gatewayCount; i++ {
		requestID := fmt.Sprintf("gateway-%d", i)
		if i%2 == 0 {
			expectedTargets[requestID] = "default/a"
		} else {
			expectedTargets[requestID] = "default/b"
		}
	}

	var mu sync.Mutex
	seen := make(map[string]int, gatewayCount)
	arrived := 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestID := req.Header.Get("X-Request-Id")
		mu.Lock()
		seen[requestID]++
		arrived++
		if arrived == gatewayCount {
			close(release)
		}
		mu.Unlock()

		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.Header().Set("Content-Type", externalMediaType)
		_, _ = w.Write([]byte(externalResponseFor(requestID, externalDecisionSelected, expectedTargets[requestID])))
	}))
	t.Cleanup(server.Close)

	type routeResult struct {
		gateway int
		address string
		err     error
	}
	start := make(chan struct{})
	results := make(chan routeResult, gatewayCount)
	routers := make([]*externalRouter, 0, gatewayCount)
	for i := 0; i < gatewayCount; i++ {
		cfg := externalRouterTestConfig(server.URL, PolicyAuthoritative, FailureFailClosed)
		cfg.maxInflight = 1
		cfg.timeout = 2 * time.Second
		client := newExternalHTTPClient(cfg)
		t.Cleanup(client.CloseIdleConnections)
		router := newExternalRouterWithDependencies(cfg, nil, client, nil, prometheus.NewRegistry())
		routers = append(routers, router)
		go func(gateway int, router *externalRouter) {
			<-start
			requestID := fmt.Sprintf("gateway-%d", gateway)
			ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", requestID, "")
			address, err := router.Route(ctx, pods)
			results <- routeResult{gateway: gateway, address: address, err: err}
		}(i, router)
	}
	close(start)

	for i := 0; i < gatewayCount; i++ {
		result := <-results
		require.NoError(t, result.err)
		if result.gateway%2 == 0 {
			require.Equal(t, "10.0.0.1:8000", result.address)
		} else {
			require.Equal(t, "10.0.0.2:8000", result.address)
		}
	}

	mu.Lock()
	require.Equal(t, gatewayCount, arrived)
	for requestID := range expectedTargets {
		require.Equal(t, 1, seen[requestID], "request %s must be decided exactly once", requestID)
	}
	mu.Unlock()
	for _, router := range routers {
		require.Equal(t, "closed", router.circuit.currentState())
		require.True(t, router.bulkhead.acquire(), "each Gateway must release its process-local bulkhead permit")
		router.bulkhead.release()
	}
}

func TestExternalCircuitIgnoresStaleResults(t *testing.T) {
	now := time.Unix(100, 0)
	circuit := newExternalCircuit(2, time.Second)
	circuit.now = func() time.Time { return now }

	lateSuccess, err := circuit.acquire()
	require.NoError(t, err)
	openingFailure, err := circuit.acquire()
	require.NoError(t, err)
	circuit.failure(openingFailure)
	thresholdFailure, err := circuit.acquire()
	require.NoError(t, err)
	circuit.failure(thresholdFailure)
	require.Equal(t, "open", circuit.currentState())

	circuit.success(lateSuccess)
	require.Equal(t, "open", circuit.currentState(), "a success from the previous generation must not close the breaker")

	now = now.Add(time.Second)
	probe, err := circuit.acquire()
	require.NoError(t, err)
	circuit.failure(openingFailure)
	_, err = circuit.acquire()
	require.ErrorIs(t, err, errExternalCircuitOpen, "a stale failure must not release the active half-open probe")

	circuit.cancel(probe)
	require.Equal(t, "open", circuit.currentState())
	_, err = circuit.acquire()
	require.ErrorIs(t, err, errExternalCircuitOpen, "cancelling a probe must restart the open cooldown")
	now = now.Add(time.Second)
	_, err = circuit.acquire()
	require.NoError(t, err)
}

func TestExternalRouterStaleSuccessKeepsOpenCircuitMetric(t *testing.T) {
	cfg := externalRouterTestConfig("http://unused.invalid", PolicyAuthoritative, FailureFailClosed)
	cfg.failureThreshold = 1
	router := newExternalRouterWithDependencies(cfg, nil, nil, nil, prometheus.NewRegistry())

	staleSuccess, err := router.circuit.acquire()
	require.NoError(t, err)
	openingFailure, err := router.circuit.acquire()
	require.NoError(t, err)
	router.circuit.failure(openingFailure)
	router.metrics.setCircuit(router.circuit.currentState())

	router.recordCircuitSuccess(staleSuccess)
	require.Equal(t, "open", router.circuit.currentState())
	require.Equal(t, float64(1), testutil.ToFloat64(router.metrics.circuit.WithLabelValues("open")))
	require.Equal(t, float64(0), testutil.ToFloat64(router.metrics.circuit.WithLabelValues("closed")))
}

func TestExternalCircuitTransitions(t *testing.T) {
	now := time.Unix(100, 0)
	circuit := newExternalCircuit(2, time.Second)
	circuit.now = func() time.Time { return now }

	first, err := circuit.acquire()
	require.NoError(t, err)
	circuit.failure(first)
	require.Equal(t, "closed", circuit.currentState())

	second, err := circuit.acquire()
	require.NoError(t, err)
	circuit.failure(second)
	require.Equal(t, "open", circuit.currentState())
	_, err = circuit.acquire()
	require.ErrorIs(t, err, errExternalCircuitOpen)

	now = now.Add(time.Second)
	probe, err := circuit.acquire()
	require.NoError(t, err)
	require.True(t, probe.halfOpen)
	_, err = circuit.acquire()
	require.ErrorIs(t, err, errExternalCircuitOpen)
	circuit.success(probe)
	require.Equal(t, "closed", circuit.currentState())
}

func TestExternalBulkhead(t *testing.T) {
	bulkhead := newExternalBulkhead(1)
	require.True(t, bulkhead.acquire())
	require.False(t, bulkhead.acquire())
	bulkhead.release()
	require.True(t, bulkhead.acquire())
	bulkhead.release()
}

func TestExternalRouterReleasesResilienceStateOnPanic(t *testing.T) {
	pod := externalTestPod("default", "a", "10.0.0.1", "zone-a")
	cfg := externalRouterTestConfig("http://unused.invalid", PolicyAuthoritative, FailureFailClosed)
	cfg.maxInflight = 1
	router := newExternalRouterWithDependencies(cfg, nil, externalPanickingDoer{}, nil, prometheus.NewRegistry())
	now := time.Unix(100, 0)
	router.circuit.now = func() time.Time { return now }
	first, err := router.circuit.acquire()
	require.NoError(t, err)
	router.circuit.failure(first)
	second, err := router.circuit.acquire()
	require.NoError(t, err)
	router.circuit.failure(second)
	now = now.Add(cfg.openDuration)

	ctx := types.NewRoutingContext(context.Background(), Algorithm, "llama", "", "panic", "")
	require.Panics(t, func() {
		_, _ = router.Route(ctx, &utils.PodArray{Pods: []*v1.Pod{pod}})
	})
	require.True(t, router.bulkhead.acquire(), "panic must release the bulkhead permit")
	router.bulkhead.release()
	require.Equal(t, float64(0), testutil.ToFloat64(router.metrics.inflight))
	require.Equal(t, "open", router.circuit.currentState(), "panic must release and cool down the half-open probe")
}

func TestExternalMetricsUseOnlyFixedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metricSet := newExternalRouterMetrics(registry)
	metricSet.requests.WithLabelValues(externalOutcomeSelected).Inc()
	metricSet.fallback.WithLabelValues(externalOutcomeTimeout).Inc()
	metricSet.duration.Observe(0.001)
	metricSet.inflight.Set(1)
	metricSet.setCircuit("open")

	families, err := registry.Gather()
	require.NoError(t, err)
	allowedLabels := map[string]struct{}{"outcome": {}, "reason": {}, "state": {}}
	found := make(map[string]bool)
	for _, family := range families {
		found[family.GetName()] = true
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				_, ok := allowedLabels[label.GetName()]
				require.True(t, ok, "unexpected external-router metric label %q", label.GetName())
			}
		}
	}
	for _, name := range []string{
		"aibrix_gateway_external_router_requests_total",
		"aibrix_gateway_external_router_fallback_total",
		"aibrix_gateway_external_router_duration_seconds",
		"aibrix_gateway_external_router_inflight",
		"aibrix_gateway_external_router_circuit_state",
	} {
		require.True(t, found[name], "missing metric family %s", name)
	}
}
