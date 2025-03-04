/*
Copyright 2024 The Aibrix Team.

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

package gateway

import (
	"os"
	"testing"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
)

func Test_ValidateRoutingStrategy(t *testing.T) {
	var tests = []struct {
		routingStrategy    string
		message            string
		expectedValidation bool
	}{
		{
			routingStrategy:    "",
			message:            "empty routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "  ",
			message:            "spaced routing strategy",
			expectedValidation: false,
		},
		{
			routingStrategy:    "random",
			message:            "random routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "least-request",
			message:            "least-request routing strategy",
			expectedValidation: true,
		},
		{
			routingStrategy:    "rrandom",
			message:            "misspell routing strategy",
			expectedValidation: false,
		},
	}

	for _, tt := range tests {
		currentValidation := validateRoutingStrategy(tt.routingStrategy)
		assert.Equal(t, tt.expectedValidation, currentValidation, tt.message)
	}
}

func TestGetRoutingStrategy(t *testing.T) {
	var tests = []struct {
		headers               []*configPb.HeaderValue
		setEnvRoutingStrategy bool
		envRoutingStrategy    string
		expectedStrategy      string
		expectedEnabled       bool
		message               string
	}{
		{
			headers:               []*configPb.HeaderValue{},
			setEnvRoutingStrategy: false,
			expectedStrategy:      "",
			expectedEnabled:       false,
			message:               "no routing strategy in headers or environment variable",
		},
		{
			headers: []*configPb.HeaderValue{
				{Key: "routing-strategy", RawValue: []byte("random")},
			},
			setEnvRoutingStrategy: false,
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "routing strategy from headers",
		},
		{
			headers:               []*configPb.HeaderValue{},
			setEnvRoutingStrategy: true,
			envRoutingStrategy:    "random",
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "routing strategy from environment variable",
		},
		{
			headers: []*configPb.HeaderValue{
				{Key: "routing-strategy", RawValue: []byte("random")},
			},
			setEnvRoutingStrategy: true,
			envRoutingStrategy:    "least-request",
			expectedStrategy:      "random",
			expectedEnabled:       true,
			message:               "header routing strategy takes priority over environment variable",
		},
	}

	for _, tt := range tests {
		if tt.setEnvRoutingStrategy {
			_ = os.Setenv("ROUTING_ALGORITHM", tt.envRoutingStrategy)
		} else {
			_ = os.Unsetenv("ROUTING_ALGORITHM")
		}

		routingStrategy, enabled := getRoutingStrategy(tt.headers)
		assert.Equal(t, tt.expectedStrategy, routingStrategy, tt.message)
		assert.Equal(t, tt.expectedEnabled, enabled, tt.message)

		// Cleanup environment variable for next test
		_ = os.Unsetenv("ROUTING_ALGORITHM")
	}
}
