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
