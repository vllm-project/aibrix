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

package e2eframework

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	t.Setenv("AIBRIX_E2E_GATEWAY_URL", "")
	t.Setenv("AIBRIX_E2E_NAMESPACE", "")
	t.Setenv("AIBRIX_E2E_API_KEY", "")
	t.Setenv("AIBRIX_E2E_GATEWAY_NAMESPACE", "")

	config := LoadConfig()

	require.Equal(t, "http://localhost:8888", config.GatewayURL)
	require.Equal(t, "default", config.Namespace)
	require.Equal(t, "test-key-1234567890", config.APIKey)
	require.Equal(t, "aibrix-system", config.GatewayNamespace)
}

func TestConfigReadsEnvironment(t *testing.T) {
	t.Setenv("AIBRIX_E2E_GATEWAY_URL", "http://gateway.example:8080")
	t.Setenv("AIBRIX_E2E_NAMESPACE", "test-namespace")
	t.Setenv("AIBRIX_E2E_API_KEY", "test-api-key")
	t.Setenv("AIBRIX_E2E_GATEWAY_NAMESPACE", "gateway-namespace")

	config := LoadConfig()

	require.Equal(t, "http://gateway.example:8080", config.GatewayURL)
	require.Equal(t, "test-namespace", config.Namespace)
	require.Equal(t, "test-api-key", config.APIKey)
	require.Equal(t, "gateway-namespace", config.GatewayNamespace)
}
