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
	"fmt"
	"os"
)

// AuthModeDev is the development auth mode name.
const AuthModeDev = "dev"

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

	// SessionSecret is used to sign session cookies. Must be provided via
	// SESSION_SECRET env var in non-dev modes; generated randomly in dev mode.
	SessionSecret string

	// SecretsEncryptionKey is the 32-byte hex-encoded AES-256 key used to
	// encrypt stored secret values. Must be provided via SECRETS_ENCRYPTION_KEY
	// env var in non-dev modes; generated randomly in dev mode.
	SecretsEncryptionKey string

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

	// AllowedOrigins is a comma-separated list of allowed CORS origins.
	// When empty, CORS is disabled (same-origin only).
	AllowedOrigins string
}

// Load reads configuration from environment variables and applies sensible defaults.
//
// In dev auth mode, SessionSecret and SecretsEncryptionKey are generated
// randomly at startup if not supplied. In non-dev modes both must be provided
// explicitly via SESSION_SECRET and SECRETS_ENCRYPTION_KEY environment
// variables; Load returns an error otherwise.
func Load() (*Config, error) {
	authMode := envOrDefault("AUTH_MODE", AuthModeDev)
	devMode := authMode == AuthModeDev

	sessionSecret, err := requiredSecret("SESSION_SECRET", devMode)
	if err != nil {
		return nil, err
	}
	encryptionKey, err := requiredSecret("SECRETS_ENCRYPTION_KEY", devMode)
	if err != nil {
		return nil, err
	}

	return &Config{
		StoreType:            envOrDefault("STORE_TYPE", "memory"),
		MySQLDSN:             envOrDefault("MYSQL_DSN", ""),
		GatewayEndpoint:      envOrDefault("GATEWAY_ENDPOINT", "http://localhost:8888"),
		MetadataServiceURL:   envOrDefault("METADATA_SERVICE_URL", "http://localhost:8000"),
		GRPCAddr:             envOrDefault("GRPC_ADDR", ":50060"),
		HTTPAddr:             envOrDefault("HTTP_ADDR", ":8090"),
		AuthMode:             authMode,
		OIDCIssuerURL:        envOrDefault("OIDC_ISSUER_URL", ""),
		OIDCClientID:         envOrDefault("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:     envOrDefault("OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:      envOrDefault("OIDC_REDIRECT_URL", "http://localhost:8090/api/v1/auth/callback"),
		SessionSecret:        sessionSecret,
		SecretsEncryptionKey: encryptionKey,
		DevUserName:          envOrDefault("DEV_USER_NAME", "Test User"),
		DevUserEmail:         envOrDefault("DEV_USER_EMAIL", "test@aibrix.ai"),
		BasicUsername:        envOrDefault("BASIC_USERNAME", ""),
		BasicPassword:        envOrDefault("BASIC_PASSWORD", ""),
		StaticFilesDir:       envOrDefault("STATIC_FILES_DIR", ""),
		AllowedOrigins:       envOrDefault("ALLOWED_ORIGINS", ""),
	}, nil
}

// envOrDefault returns the value of the environment variable named by key,
// or fallback if the variable is not set or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requiredSecret returns the env var value if set. In dev mode it generates a
// random 32-byte hex value when unset. In non-dev mode it returns an error
// when the env var is empty.
func requiredSecret(key string, devMode bool) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	if !devMode {
		return "", fmt.Errorf("%s must be set in non-dev auth modes", key)
	}
	return generateRandomHex(32), nil
}

// generateRandomHex returns a hex-encoded random string of n bytes. It panics
// if crypto/rand fails, since continuing without strong randomness would
// compromise any security-sensitive use of the result.
func generateRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}
