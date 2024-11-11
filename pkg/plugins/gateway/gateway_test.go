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
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_ValidateRoutingStrategy(t *testing.T) {
	var tests = []struct {
		headerBasedRoutingStrategyEnabled bool
		routingStrategy                   string
		setEnvRoutingStrategy             bool
		message                           string
		expectedValidation                bool
	}{
		{
			headerBasedRoutingStrategyEnabled: true,
			routingStrategy:                   "",
			setEnvRoutingStrategy:             false,
			message:                           "empty routing strategy in header",
			expectedValidation:                false,
		},
		{
			headerBasedRoutingStrategyEnabled: true,
			routingStrategy:                   "  ",
			setEnvRoutingStrategy:             false,
			message:                           "spaced routing strategy in header",
			expectedValidation:                false,
		},
		{
			headerBasedRoutingStrategyEnabled: false,
			routingStrategy:                   "",
			setEnvRoutingStrategy:             true,
			message:                           "empty routing strategy in header with env var set",
			expectedValidation:                true,
		},
		{
			headerBasedRoutingStrategyEnabled: false,
			routingStrategy:                   "  ",
			setEnvRoutingStrategy:             true,
			message:                           "spaced routing strategy in header with env var set",
			expectedValidation:                true,
		},
		{
			headerBasedRoutingStrategyEnabled: true,
			routingStrategy:                   "least-request",
			setEnvRoutingStrategy:             false,
			message:                           "header routing strategy least-request",
			expectedValidation:                true,
		},
		{
			headerBasedRoutingStrategyEnabled: true,
			routingStrategy:                   "rrandom",
			setEnvRoutingStrategy:             false,
			message:                           "header routing strategy invalid",
			expectedValidation:                false,
		},
		{
			headerBasedRoutingStrategyEnabled: false,
			routingStrategy:                   "random",
			setEnvRoutingStrategy:             true,
			message:                           "env routing strategy",
			expectedValidation:                true,
		},
		{
			headerBasedRoutingStrategyEnabled: false,
			routingStrategy:                   "rrandom",
			setEnvRoutingStrategy:             true,
			message:                           "incorrect env routing strategy",
			expectedValidation:                false,
		},
		{
			headerBasedRoutingStrategyEnabled: true,
			routingStrategy:                   "random",
			setEnvRoutingStrategy:             true,
			message:                           "per request overrides env",
			expectedValidation:                true,
		},
	}

	for _, tt := range tests {
		if tt.setEnvRoutingStrategy {
			t.Setenv("ROUTING_ALGORITHM", "least-request")
		}

		currentValidation := validateRoutingStrategy(tt.routingStrategy, tt.headerBasedRoutingStrategyEnabled)
		assert.Equal(t, tt.expectedValidation, currentValidation, tt.message)
	}
}
