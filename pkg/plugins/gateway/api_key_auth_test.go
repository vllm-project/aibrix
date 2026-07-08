package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAPIKeyAuthConfigFromEnv(t *testing.T) {
	t.Setenv(envAuthBearerToken, "token-a")

	cfg := loadAPIKeyAuthConfig()

	assert.Equal(t, "token-a", cfg.token)
}

func TestExtractBearerToken(t *testing.T) {
	token, ok := extractBearerToken("Bearer token-a")
	require.True(t, ok)
	assert.Equal(t, "token-a", token)

	_, ok = extractBearerToken("token-a")
	assert.False(t, ok)
}
