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

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
)

func captureKlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
	return &logs
}

func TestGatewayServerOptionsFromEnv(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset"},
		{name: "false", value: "false", set: true},
		{name: "true", value: "true", set: true, want: true},
		{name: "invalid uses default", value: "not-a-bool", set: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureKlog(t)
			if tt.set {
				t.Setenv(envDisableRateLimiting, tt.value)
			} else {
				t.Setenv(envDisableRateLimiting, "")
			}

			if got := gatewayServerOptionsFromEnv().DisableRateLimiting; got != tt.want {
				t.Fatalf("DisableRateLimiting = %t, want %t", got, tt.want)
			}
			if tt.name == "invalid uses default" {
				got := logs.String()
				if !strings.Contains(got, "invalid "+envDisableRateLimiting) ||
					!strings.Contains(got, "falling back to default: false") {
					t.Fatalf("invalid value warning = %q, want env name and false fallback", got)
				}
			}
		})
	}
}

func TestLogGatewayRateLimitingMode(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options gateway.ServerOptions
		want    string
	}{
		{name: "enabled", want: "enabled=true"},
		{name: "disabled", options: gateway.ServerOptions{DisableRateLimiting: true}, want: "enabled=false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureKlog(t)
			logGatewayRateLimitingMode(tt.options)
			got := logs.String()
			if !strings.Contains(got, "gateway rate limiting configured") || !strings.Contains(got, tt.want) {
				t.Fatalf("effective-mode log = %q, want message containing %q", got, tt.want)
			}
		})
	}
}
