/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package external

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"go.opentelemetry.io/otel/attribute"
)

const Algorithm types.RoutingAlgorithm = "external"

var (
	ErrExternalPolicyDenied      = errors.New("external routing policy denied")
	ErrExternalRouterUnavailable = errors.New("external router unavailable")
)

type externalRouter struct {
	cfg            externalRouterConfig
	cache          cache.Cache
	client         externalHTTPDoer
	selectFallback func(*types.RoutingContext) (types.Router, error)
	bulkhead       *externalBulkhead
	circuit        *externalCircuit
	metrics        *externalRouterMetrics
}

func NewRouter(metricCache cache.Cache, selector func(*types.RoutingContext) (types.Router, error)) (types.Router, error) {
	return newExternalRouterWithCacheAndSelector(metricCache, selector)
}

func newExternalRouterWithCacheAndSelector(metricCache cache.Cache, selector func(*types.RoutingContext) (types.Router, error)) (types.Router, error) {
	cfg, err := loadExternalRouterConfig()
	if err != nil {
		return nil, err
	}
	router := newExternalRouterWithDependencies(cfg, metricCache, newExternalHTTPClient(cfg), selector, prometheus.DefaultRegisterer)
	if metricCache != nil && len(router.SubscribedMetrics()) > 0 {
		metricCache.AddSubscriber(router)
	}
	return router, nil
}

func newExternalRouterWithDependencies(
	cfg externalRouterConfig,
	metricCache cache.Cache,
	client externalHTTPDoer,
	selector func(*types.RoutingContext) (types.Router, error),
	registerer prometheus.Registerer,
) *externalRouter {
	return &externalRouter{
		cfg:            cfg,
		cache:          metricCache,
		client:         client,
		selectFallback: selector,
		bulkhead:       newExternalBulkhead(cfg.maxInflight),
		circuit:        newExternalCircuit(cfg.failureThreshold, cfg.openDuration),
		metrics:        newExternalRouterMetrics(registerer),
	}
}

func (r *externalRouter) BypassSingleCandidate() bool {
	return r.cfg.policyMode == PolicyAdvisory
}

func (r *externalRouter) ConfiguredFallback() types.RoutingAlgorithm {
	return r.cfg.fallback
}

func (r *externalRouter) SubscribedMetrics() []string {
	subscribed := make([]string, 0, 2)
	if _, ok := r.cfg.candidateMetrics[externalMetricEngineUtilization]; ok {
		subscribed = append(subscribed, metrics.EngineUtilization)
	}
	if _, ok := r.cfg.candidateMetrics[externalMetricKVCacheUsage]; ok {
		subscribed = append(subscribed, metrics.KVCacheUsagePerc)
	}
	return subscribed
}

func (r *externalRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	request, snapshots, err := buildExternalDecisionRequest(r.cfg, r.cache, ctx, pods)
	if err != nil {
		return r.handleFailure(ctx, pods, externalOutcomeInvalidResponse, err)
	}

	circuitToken, err := r.circuit.acquire()
	if err != nil {
		r.metrics.setCircuit(r.circuit.currentState())
		return r.handleFailure(ctx, pods, externalOutcomeCircuitOpen, err)
	}
	r.metrics.setCircuit(r.circuit.currentState())
	circuitSettled := false
	defer func() {
		if !circuitSettled {
			r.circuit.cancel(circuitToken)
			r.metrics.setCircuit(r.circuit.currentState())
		}
	}()
	if !r.bulkhead.acquire() {
		r.circuit.cancel(circuitToken)
		circuitSettled = true
		return r.handleFailure(ctx, pods, externalOutcomeBulkheadRejected, errExternalBulkheadRejected)
	}
	var body []byte
	var duration time.Duration
	var exchangeErr error
	func() {
		r.metrics.inflight.Inc()
		defer r.bulkhead.release()
		defer r.metrics.inflight.Dec()
		body, duration, exchangeErr = executeExternalRequest(ctx.Context, r.client, r.cfg, request, ctx.ReqHeaders["traceparent"])
	}()
	if duration > 0 {
		r.metrics.duration.Observe(duration.Seconds())
	}
	if exchangeErr != nil {
		var classified *externalExchangeError
		if errors.As(exchangeErr, &classified) {
			if classified.outcome == externalOutcomeCancelled {
				r.circuit.cancel(circuitToken)
			} else if classified.attempted {
				r.circuit.failure(circuitToken)
			} else {
				r.circuit.cancel(circuitToken)
			}
			circuitSettled = true
			r.metrics.setCircuit(r.circuit.currentState())
			return r.handleFailure(ctx, pods, classified.outcome, exchangeErr)
		}
		r.circuit.failure(circuitToken)
		circuitSettled = true
		r.metrics.setCircuit(r.circuit.currentState())
		return r.handleFailure(ctx, pods, externalOutcomeTransportError, exchangeErr)
	}

	decision, err := validateExternalDecision(body, ctx.RequestID, r.cfg.policyMode, snapshots)
	if err != nil {
		r.circuit.failure(circuitToken)
		circuitSettled = true
		r.metrics.setCircuit(r.circuit.currentState())
		return r.handleFailure(ctx, pods, externalOutcomeInvalidResponse, err)
	}
	r.recordCircuitSuccess(circuitToken)
	circuitSettled = true
	if ctx.Span != nil {
		attrs := []attribute.KeyValue{
			attribute.String("external_router.policy_mode", string(r.cfg.policyMode)),
			attribute.String("external_router.decision", decision.decision),
			attribute.Bool("external_router.fallback", decision.decision == externalDecisionNoDecision),
		}
		if decision.decisionID != "" {
			attrs = append(attrs, attribute.String("external_router.decision_id", decision.decisionID))
		}
		ctx.Span.SetAttributes(attrs...)
	}

	switch decision.decision {
	case externalDecisionSelected:
		r.metrics.requests.WithLabelValues(externalOutcomeSelected).Inc()
		ctx.SetTargetPort(decision.targetPort)
		ctx.SetTargetPod(decision.targetPod)
		return ctx.TargetAddress(), nil
	case externalDecisionNoDecision:
		r.metrics.requests.WithLabelValues(externalOutcomeNoDecision).Inc()
		return r.routeFallback(ctx, pods, externalOutcomeNoDecision)
	case externalDecisionDenied:
		r.metrics.requests.WithLabelValues(externalOutcomeDenied).Inc()
		return "", ErrExternalPolicyDenied
	default:
		return r.handleFailure(ctx, pods, externalOutcomeInvalidResponse, errors.New("unreachable external decision"))
	}
}

func (r *externalRouter) recordCircuitSuccess(token externalCircuitToken) {
	r.circuit.success(token)
	r.metrics.setCircuit(r.circuit.currentState())
}

func (r *externalRouter) handleFailure(ctx *types.RoutingContext, pods types.PodList, outcome string, cause error) (string, error) {
	r.metrics.requests.WithLabelValues(outcome).Inc()
	if outcome == externalOutcomeCancelled {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", cause
	}
	if r.cfg.failureMode == FailureFailOpen {
		return r.routeFallback(ctx, pods, outcome)
	}
	return "", fmt.Errorf("%w: %v", ErrExternalRouterUnavailable, cause)
}

func (r *externalRouter) routeFallback(ctx *types.RoutingContext, pods types.PodList, reason string) (string, error) {
	if r.cfg.fallback == "" || r.cfg.fallback == Algorithm || r.selectFallback == nil {
		return "", fmt.Errorf("%w: fallback is unavailable", ErrExternalRouterUnavailable)
	}
	r.metrics.fallback.WithLabelValues(reason).Inc()
	original := ctx.Algorithm
	ctx.Algorithm = r.cfg.fallback
	fallback, err := r.selectFallback(ctx)
	if err == nil && fallback != nil {
		var address string
		address, err = fallback.Route(ctx, pods)
		ctx.Algorithm = original
		if err == nil {
			return address, nil
		}
	} else {
		ctx.Algorithm = original
		if err == nil {
			err = errors.New("fallback provider returned nil router")
		}
	}
	ctx.Algorithm = original
	return "", fmt.Errorf("%w: fallback %s failed: %v", ErrExternalRouterUnavailable, r.cfg.fallback, err)
}

var _ types.Router = (*externalRouter)(nil)
var _ types.SingleCandidateBypasser = (*externalRouter)(nil)
var _ metrics.MetricSubscriber = (*externalRouter)(nil)
