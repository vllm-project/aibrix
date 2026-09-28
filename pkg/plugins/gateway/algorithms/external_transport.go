/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package routingalgorithms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"time"
)

var externalTraceparentRE = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

const (
	externalOutcomeSelected         = "selected"
	externalOutcomeNoDecision       = "no_decision"
	externalOutcomeDenied           = "denied"
	externalOutcomeTimeout          = "timeout"
	externalOutcomeTransportError   = "transport_error"
	externalOutcomeHTTPError        = "http_error"
	externalOutcomeInvalidResponse  = "invalid_response"
	externalOutcomeRequestTooLarge  = "request_too_large"
	externalOutcomeBulkheadRejected = "bulkhead_rejected"
	externalOutcomeCircuitOpen      = "circuit_open"
	externalOutcomeCancelled        = "cancelled"
)

type externalHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type externalExchangeError struct {
	outcome   string
	attempted bool
	err       error
}

func (e *externalExchangeError) Error() string { return e.err.Error() }
func (e *externalExchangeError) Unwrap() error { return e.err }

func newExternalHTTPClient(cfg externalRouterConfig) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxIdleConns = cfg.maxInflight
	transport.MaxIdleConnsPerHost = cfg.maxInflight
	transport.MaxConnsPerHost = cfg.maxInflight
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func executeExternalRequest(parent context.Context, client externalHTTPDoer, cfg externalRouterConfig, request externalDecisionRequest, traceparent string) ([]byte, time.Duration, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, 0, &externalExchangeError{outcome: externalOutcomeInvalidResponse, err: fmt.Errorf("encode external request: %w", err)}
	}
	if int64(len(body)) > cfg.maxRequestBytes {
		return nil, 0, &externalExchangeError{outcome: externalOutcomeRequestTooLarge, err: errors.New("external routing request exceeds configured limit")}
	}

	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, &externalExchangeError{outcome: externalOutcomeTransportError, err: fmt.Errorf("create external request: %w", err)}
	}
	httpRequest.Header.Set("Content-Type", externalMediaType)
	httpRequest.Header.Set("Accept", externalMediaType)
	httpRequest.Header.Set("X-Request-Id", request.Metadata.RequestID)
	if validExternalTraceparent(traceparent) {
		httpRequest.Header.Set("traceparent", traceparent)
	}
	if cfg.bearerToken != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+cfg.bearerToken)
	}

	start := time.Now()
	response, err := client.Do(httpRequest)
	duration := time.Since(start)
	if err != nil {
		outcome := externalOutcomeTransportError
		if errors.Is(parent.Err(), context.Canceled) || errors.Is(parent.Err(), context.DeadlineExceeded) {
			outcome = externalOutcomeCancelled
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = externalOutcomeTimeout
		}
		return nil, duration, &externalExchangeError{outcome: outcome, attempted: true, err: fmt.Errorf("external routing exchange: %w", err)}
	}
	defer func() { _ = response.Body.Close() }()

	bounded, readErr := io.ReadAll(io.LimitReader(response.Body, cfg.maxResponseBytes+1))
	if readErr != nil {
		return nil, duration, &externalExchangeError{outcome: externalOutcomeTransportError, attempted: true, err: fmt.Errorf("read external response: %w", readErr)}
	}
	if int64(len(bounded)) > cfg.maxResponseBytes {
		return nil, duration, &externalExchangeError{outcome: externalOutcomeInvalidResponse, attempted: true, err: errors.New("external response exceeds configured limit")}
	}
	if response.StatusCode != http.StatusOK {
		return nil, duration, &externalExchangeError{outcome: externalOutcomeHTTPError, attempted: true, err: fmt.Errorf("external service returned HTTP %d", response.StatusCode)}
	}
	mediaType, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || mediaType != "application/vnd.aibrix.external-routing+json" || params["version"] != "v1alpha1" {
		return nil, duration, &externalExchangeError{outcome: externalOutcomeInvalidResponse, attempted: true, err: errors.New("external response has invalid content type")}
	}
	return bounded, duration, nil
}

func validExternalTraceparent(value string) bool {
	if !externalTraceparentRE.MatchString(value) || value[:2] == "ff" {
		return false
	}
	return value[3:35] != "00000000000000000000000000000000" && value[36:52] != "0000000000000000"
}
