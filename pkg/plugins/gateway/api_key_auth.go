package gateway

import (
	"os"
	"strings"
)

const (
	envAuthBearerToken = "AIBRIX_AUTH_BEARER_TOKEN"
)

type apiKeyAuthConfig struct {
	token string
}

func loadAPIKeyAuthConfig() *apiKeyAuthConfig {
	return &apiKeyAuthConfig{
		token: strings.TrimSpace(os.Getenv(envAuthBearerToken)),
	}
}

func (c *apiKeyAuthConfig) isAuthorized(token string) bool {
	token, ok := extractBearerToken(token)
	if !ok {
		return false
	}
	return c.token == token
}

func extractBearerToken(headerValue string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(headerValue), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}
