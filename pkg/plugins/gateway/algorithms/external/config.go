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
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
)

const (
	EnvExternalRouterEndpoint            = "AIBRIX_EXTERNAL_ROUTER_ENDPOINT"
	EnvExternalRouterPolicyMode          = "AIBRIX_EXTERNAL_ROUTER_POLICY_MODE"
	EnvExternalRouterFailureMode         = "AIBRIX_EXTERNAL_ROUTER_FAILURE_MODE"
	EnvExternalRouterFallback            = "AIBRIX_EXTERNAL_ROUTER_FALLBACK"
	EnvExternalRouterTimeout             = "AIBRIX_EXTERNAL_ROUTER_TIMEOUT"
	EnvExternalRouterMaxInflight         = "AIBRIX_EXTERNAL_ROUTER_MAX_INFLIGHT"
	EnvExternalRouterMaxRequestBytes     = "AIBRIX_EXTERNAL_ROUTER_MAX_REQUEST_BYTES"
	EnvExternalRouterMaxResponseBytes    = "AIBRIX_EXTERNAL_ROUTER_MAX_RESPONSE_BYTES"
	EnvExternalRouterFailureThreshold    = "AIBRIX_EXTERNAL_ROUTER_FAILURE_THRESHOLD"
	EnvExternalRouterOpenDuration        = "AIBRIX_EXTERNAL_ROUTER_OPEN_DURATION"
	EnvExternalRouterAuthTokenFile       = "AIBRIX_EXTERNAL_ROUTER_AUTH_TOKEN_FILE"
	EnvExternalRouterCandidateAttributes = "AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES"
	EnvExternalRouterCandidateMetrics    = "AIBRIX_EXTERNAL_ROUTER_CANDIDATE_METRICS"
	EnvExternalRouterPolicyAttributes    = "AIBRIX_EXTERNAL_ROUTER_POLICY_ATTRIBUTES"

	externalMetricRunningRequests   = "runningRequests"
	externalMetricEngineUtilization = "engineUtilization"
	externalMetricKVCacheUsage      = "kvCacheUsage"
)

var (
	ErrExternalRouterDisabled = errors.New("external router is disabled")
	externalAttributeKeyRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./-]{0,127}$`)
)

type externalPolicyMode string

const (
	PolicyAdvisory      externalPolicyMode = "Advisory"
	PolicyAuthoritative externalPolicyMode = "Authoritative"
)

type externalFailureMode string

const (
	FailureFailOpen   externalFailureMode = "FailOpen"
	FailureFailClosed externalFailureMode = "FailClosed"
)

type externalRouterConfig struct {
	endpoint            *url.URL
	policyMode          externalPolicyMode
	failureMode         externalFailureMode
	fallback            types.RoutingAlgorithm
	timeout             time.Duration
	maxInflight         int
	maxRequestBytes     int64
	maxResponseBytes    int64
	failureThreshold    int
	openDuration        time.Duration
	bearerToken         string
	candidateAttributes []string
	candidateMetrics    map[string]struct{}
	policyAttributes    []string
}

func loadExternalRouterConfig() (externalRouterConfig, error) {
	return loadExternalRouterConfigFrom(os.Getenv, os.ReadFile)
}

//nolint:gocyclo // Keeping the cross-field configuration contract in one validator prevents modes and limits from drifting.
func loadExternalRouterConfigFrom(getenv func(string) string, readFile func(string) ([]byte, error)) (externalRouterConfig, error) {
	cfg := externalRouterConfig{
		timeout:          10 * time.Millisecond,
		maxInflight:      256,
		maxRequestBytes:  256 * 1024,
		maxResponseBytes: 64 * 1024,
		failureThreshold: 5,
		openDuration:     time.Second,
		candidateMetrics: make(map[string]struct{}),
	}
	rawEndpoint := strings.TrimSpace(getenv(EnvExternalRouterEndpoint))
	if rawEndpoint == "" {
		return cfg, ErrExternalRouterDisabled
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || !endpoint.IsAbs() || endpoint.Host == "" {
		return cfg, fmt.Errorf("%s must be a complete absolute URL", EnvExternalRouterEndpoint)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return cfg, fmt.Errorf("%s scheme must be http or https", EnvExternalRouterEndpoint)
	}
	if endpoint.User != nil || endpoint.Fragment != "" {
		return cfg, fmt.Errorf("%s must not contain userinfo or fragment", EnvExternalRouterEndpoint)
	}
	cfg.endpoint = endpoint

	cfg.policyMode = externalPolicyMode(strings.TrimSpace(getenv(EnvExternalRouterPolicyMode)))
	if cfg.policyMode != PolicyAdvisory && cfg.policyMode != PolicyAuthoritative {
		return cfg, fmt.Errorf("%s must be Advisory or Authoritative", EnvExternalRouterPolicyMode)
	}
	cfg.failureMode = externalFailureMode(strings.TrimSpace(getenv(EnvExternalRouterFailureMode)))
	if cfg.failureMode != FailureFailOpen && cfg.failureMode != FailureFailClosed {
		return cfg, fmt.Errorf("%s must be FailOpen or FailClosed", EnvExternalRouterFailureMode)
	}
	if cfg.policyMode == PolicyAuthoritative && cfg.failureMode != FailureFailClosed {
		return cfg, errors.New("authoritative external routing requires FailClosed")
	}

	fallback := strings.TrimSpace(getenv(EnvExternalRouterFallback))
	if fallback != "" {
		cfg.fallback = types.RoutingAlgorithm(fallback)
		if cfg.fallback == Algorithm {
			return cfg, errors.New("external router cannot fall back to itself")
		}
	}
	if cfg.policyMode == PolicyAdvisory || cfg.failureMode == FailureFailOpen {
		if cfg.fallback == "" {
			return cfg, fmt.Errorf("%s is required for Advisory or FailOpen", EnvExternalRouterFallback)
		}
	}

	if cfg.timeout, err = loadPositiveDuration(getenv, EnvExternalRouterTimeout, cfg.timeout); err != nil {
		return cfg, err
	}
	if cfg.openDuration, err = loadPositiveDuration(getenv, EnvExternalRouterOpenDuration, cfg.openDuration); err != nil {
		return cfg, err
	}
	if cfg.maxInflight, err = loadPositiveInt(getenv, EnvExternalRouterMaxInflight, cfg.maxInflight); err != nil {
		return cfg, err
	}
	if cfg.failureThreshold, err = loadPositiveInt(getenv, EnvExternalRouterFailureThreshold, cfg.failureThreshold); err != nil {
		return cfg, err
	}
	if cfg.maxRequestBytes, err = loadPositiveBytes(getenv, EnvExternalRouterMaxRequestBytes, cfg.maxRequestBytes); err != nil {
		return cfg, err
	}
	if cfg.maxResponseBytes, err = loadPositiveBytes(getenv, EnvExternalRouterMaxResponseBytes, cfg.maxResponseBytes); err != nil {
		return cfg, err
	}
	if cfg.candidateAttributes, err = parseExternalAllowlist(getenv(EnvExternalRouterCandidateAttributes)); err != nil {
		return cfg, fmt.Errorf("%s: %w", EnvExternalRouterCandidateAttributes, err)
	}
	if cfg.policyAttributes, err = parseExternalAllowlist(getenv(EnvExternalRouterPolicyAttributes)); err != nil {
		return cfg, fmt.Errorf("%s: %w", EnvExternalRouterPolicyAttributes, err)
	}
	metricNames, err := parseExternalAllowlist(getenv(EnvExternalRouterCandidateMetrics))
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", EnvExternalRouterCandidateMetrics, err)
	}
	for _, name := range metricNames {
		switch name {
		case externalMetricRunningRequests, externalMetricEngineUtilization, externalMetricKVCacheUsage:
			cfg.candidateMetrics[name] = struct{}{}
		default:
			return cfg, fmt.Errorf("%s contains unsupported metric %q", EnvExternalRouterCandidateMetrics, name)
		}
	}

	tokenFile := strings.TrimSpace(getenv(EnvExternalRouterAuthTokenFile))
	if tokenFile != "" {
		token, readErr := readFile(tokenFile)
		if readErr != nil {
			return cfg, fmt.Errorf("read %s: %w", EnvExternalRouterAuthTokenFile, readErr)
		}
		cfg.bearerToken = strings.TrimSpace(string(token))
		if cfg.bearerToken == "" {
			return cfg, fmt.Errorf("%s contains an empty token", EnvExternalRouterAuthTokenFile)
		}
		if len(cfg.bearerToken) > 8192 || strings.ContainsAny(cfg.bearerToken, "\r\n") {
			return cfg, fmt.Errorf("%s contains an invalid token", EnvExternalRouterAuthTokenFile)
		}
	}
	return cfg, nil
}

func loadPositiveDuration(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func loadPositiveInt(getenv func(string) string, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func loadPositiveBytes(getenv func(string) string, key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	multiplier := int64(1)
	number := raw
	for suffix, factor := range map[string]int64{"KiB": 1024, "MiB": 1024 * 1024} {
		if strings.HasSuffix(raw, suffix) {
			multiplier = factor
			number = strings.TrimSpace(strings.TrimSuffix(raw, suffix))
			break
		}
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value <= 0 || value > (1<<62)/multiplier {
		return 0, fmt.Errorf("%s must be a positive byte size", key)
	}
	return value * multiplier, nil
}

func parseExternalAllowlist(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		value := strings.TrimSpace(item)
		if !externalAttributeKeyRE.MatchString(value) {
			return nil, fmt.Errorf("invalid name %q", value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate name %q", value)
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) > 32 {
		return nil, errors.New("allowlist contains more than 32 names")
	}
	sort.Strings(values)
	return values, nil
}
