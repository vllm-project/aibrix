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
