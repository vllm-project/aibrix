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

package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
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
	cTokenHash := sha256.Sum256([]byte(c.token))
	tokenHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(cTokenHash[:], tokenHash[:]) == 1
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
