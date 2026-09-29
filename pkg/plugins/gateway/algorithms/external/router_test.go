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
