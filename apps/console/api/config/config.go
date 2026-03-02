/*
Copyright 2025 The Aibrix Team.

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

package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

// Config holds all configuration for the AIBrix console backend.
type Config struct {
	// StoreType selects the backing store: "memory" or "mysql".
	StoreType string
	// MySQLDSN is the MySQL connection string (used when StoreType is "mysql").
	MySQLDSN string

	// GatewayEndpoint is the AIBrix gateway URL for proxying inference requests.
	GatewayEndpoint string
	// MetadataServiceURL is the metadata service URL for file proxy operations.
	MetadataServiceURL string

	// GRPCAddr is the listen address for the gRPC server.
	GRPCAddr string
	// HTTPAddr is the listen address for the HTTP gateway.
	HTTPAddr string

	// AuthMode controls authentication: "dev", "oidc", or "basic".
	AuthMode string
	// OIDCIssuerURL is the OIDC provider issuer URL.
	OIDCIssuerURL string
	// OIDCClientID is the OIDC client identifier.
	OIDCClientID string
	// OIDCClientSecret is the OIDC client secret.
	OIDCClientSecret string
	// OIDCRedirectURL is the OIDC redirect URL after authentication.
	OIDCRedirectURL string

	// SessionSecret is used to sign session cookies.
	SessionSecret string

	// DevUserName is the display name used in dev auth mode.
	DevUserName string
	// DevUserEmail is the email address used in dev auth mode.
	DevUserEmail string

	// BasicUsername is the username for basic auth mode.
	BasicUsername string
	// BasicPassword is the password for basic auth mode.
	BasicPassword string

	// StaticFilesDir is the path to the frontend dist/ directory.
	// When empty, static file serving is disabled.
	StaticFilesDir string
}

// Load reads configuration from environment variables and applies sensible defaults.
func Load() *Config {
	return &Config{
		StoreType:          envOrDefault("STORE_TYPE", "memory"),
		MySQLDSN:           envOrDefault("MYSQL_DSN", ""),
		GatewayEndpoint:    envOrDefault("GATEWAY_ENDPOINT", "http://localhost:8888"),
		MetadataServiceURL: envOrDefault("METADATA_SERVICE_URL", "http://localhost:8000"),
		GRPCAddr:           envOrDefault("GRPC_ADDR", ":50060"),
		HTTPAddr:           envOrDefault("HTTP_ADDR", ":8090"),
		AuthMode:           envOrDefault("AUTH_MODE", "dev"),
		OIDCIssuerURL:      envOrDefault("OIDC_ISSUER_URL", ""),
		OIDCClientID:       envOrDefault("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:   envOrDefault("OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:    envOrDefault("OIDC_REDIRECT_URL", "http://localhost:8090/api/v1/auth/callback"),
		SessionSecret:      envOrDefault("SESSION_SECRET", generateRandomHex(32)),
		DevUserName:        envOrDefault("DEV_USER_NAME", "Test User"),
		DevUserEmail:       envOrDefault("DEV_USER_EMAIL", "test@aibrix.ai"),
		BasicUsername:      envOrDefault("BASIC_USERNAME", ""),
		BasicPassword:      envOrDefault("BASIC_PASSWORD", ""),
		StaticFilesDir:     envOrDefault("STATIC_FILES_DIR", ""),
	}
}

// envOrDefault returns the value of the environment variable named by key,
// or fallback if the variable is not set or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// generateRandomHex returns a hex-encoded random string of n bytes.
func generateRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a fixed value if crypto/rand fails (should not happen).
		return "aibrix-console-default-session-secret"
	}
	return hex.EncodeToString(b)
}
