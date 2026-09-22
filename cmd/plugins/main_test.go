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
	"flag"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

func TestKubeAPIFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantQPS   float64
		wantBurst int
		parseErr  bool
		wantErr   string
	}{
		{name: "defaults", wantQPS: float64(rest.DefaultQPS), wantBurst: rest.DefaultBurst},
		{name: "custom", args: []string{"--kube-api-qps=25.5", "--kube-api-burst=50"}, wantQPS: 25.5, wantBurst: 50},
		{name: "QPS only", args: []string{"--kube-api-qps=0.5"}, wantQPS: 0.5, wantBurst: rest.DefaultBurst},
		{name: "burst only", args: []string{"--kube-api-burst=1"}, wantQPS: float64(rest.DefaultQPS), wantBurst: 1},
		{name: "zero QPS", args: []string{"--kube-api-qps=0"}, wantErr: "--kube-api-qps"},
		{name: "negative QPS", args: []string{"--kube-api-qps=-1"}, wantErr: "--kube-api-qps"},
		{name: "zero burst", args: []string{"--kube-api-burst=0"}, wantErr: "--kube-api-burst"},
		{name: "negative burst", args: []string{"--kube-api-burst=-1"}, wantErr: "--kube-api-burst"},
		{name: "NaN QPS", args: []string{"--kube-api-qps=NaN"}, wantErr: "--kube-api-qps"},
		{name: "infinite QPS", args: []string{"--kube-api-qps=+Inf"}, wantErr: "--kube-api-qps"},
		{name: "overflow QPS", args: []string{"--kube-api-qps=1e39"}, wantErr: "--kube-api-qps"},
		{name: "underflow QPS", args: []string{"--kube-api-qps=1e-50"}, wantErr: "--kube-api-qps"},
		{name: "malformed QPS", args: []string{"--kube-api-qps=invalid"}, parseErr: true},
		{name: "fractional burst", args: []string{"--kube-api-burst=1.5"}, parseErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var options kubeAPIOptions
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			options.addFlags(fs)
			err := fs.Parse(tt.args)
			if tt.parseErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.wantErr != "" {
				require.ErrorContains(t, options.validate(), tt.wantErr)
				return
			}
			require.NoError(t, options.validate())
			require.Equal(t, tt.wantQPS, options.qps)
			require.Equal(t, tt.wantBurst, options.burst)
		})
	}
}

func TestKubeAPIClientConfiguration(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--kube-api-qps=25.5", "--kube-api-burst=50"},
		{"--kube-api-qps=0.000001", "--kube-api-burst=3"},
	} {
		t.Run("flags="+strings.Join(args, ","), func(t *testing.T) {
			var options kubeAPIOptions
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			options.addFlags(fs)
			require.NoError(t, fs.Parse(args))
			require.NoError(t, options.validate())

			// Client construction does not contact the API server.
			config := &rest.Config{Host: "https://localhost", QPS: 99, Burst: 99}
			options.applyTo(config)
			coreClient, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			gatewayClient, err := versioned.NewForConfig(config)
			require.NoError(t, err)
			require.Equal(t, float32(options.qps), config.QPS)
			require.Equal(t, options.burst, config.Burst)
			require.Equal(t, "https://localhost", config.Host)

			for name, client := range map[string]rest.Interface{
				"core":    coreClient.CoreV1().RESTClient(),
				"gateway": gatewayClient.GatewayV1().RESTClient(),
			} {
				t.Run(name, func(t *testing.T) {
					limiter := client.GetRateLimiter()
					require.NotNil(t, limiter)
					require.Equal(t, config.QPS, limiter.QPS())
					// A very low QPS prevents token refill during this burst check.
					if options.qps == 0.000001 {
						for i := 0; i < options.burst; i++ {
							require.True(t, limiter.TryAccept(), "burst token %d", i)
						}
						require.False(t, limiter.TryAccept(), "configured burst must be exhausted")
					}
				})
			}
		})
	}
}
