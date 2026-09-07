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

import "os"

const (
	defaultGatewayURL       = "http://localhost:8888"
	defaultNamespace        = "default"
	defaultAPIKey           = "test-key-1234567890"
	defaultGatewayNamespace = "aibrix-system"
)

// Config holds the environment-dependent endpoints and namespaces for live-cluster e2e tests.
type Config struct {
	GatewayURL       string
	Namespace        string
	APIKey           string
	GatewayNamespace string
}

// LoadConfig reads e2e configuration while preserving the previous local Kind defaults.
func LoadConfig() Config {
	return Config{
		GatewayURL:       envOrDefault("AIBRIX_E2E_GATEWAY_URL", defaultGatewayURL),
		Namespace:        envOrDefault("AIBRIX_E2E_NAMESPACE", defaultNamespace),
		APIKey:           envOrDefault("AIBRIX_E2E_API_KEY", defaultAPIKey),
		GatewayNamespace: envOrDefault("AIBRIX_E2E_GATEWAY_NAMESPACE", defaultGatewayNamespace),
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
