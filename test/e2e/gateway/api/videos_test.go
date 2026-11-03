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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	asyncVideoPublicIDPrefix = "aibrixjob-"
	asyncVideoRequestTimeout = 30 * time.Second
)

type videoE2EResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	MockPod string `json:"mock_pod"`
	Deleted bool   `json:"deleted"`
}

type videoE2EListResponse struct {
	Object  string             `json:"object"`
	Data    []videoE2EResponse `json:"data"`
	HasMore bool               `json:"has_more"`
}

func doVideoE2ERequest(t *testing.T, method, path, user, contentType string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), asyncVideoRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, gatewayURL+path, body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if user != "" {
		req.Header.Set("user", user)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	payload, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	return resp, payload
}

func TestAsyncVideoLifecycle(t *testing.T) {
	owner := fmt.Sprintf("video-e2e-%d", time.Now().UnixNano())
	otherOwner := owner + "-other"
	redisClient := utils.GetRedisClient()
	require.NotNil(t, redisClient, "Redis must be available for owner-scoped video requests")
	t.Cleanup(func() {
		_ = utils.DelUser(context.Background(), utils.User{Name: owner}, redisClient)
		_ = utils.DelUser(context.Background(), utils.User{Name: otherOwner}, redisClient)
		_ = redisClient.Close()
	})
	for _, name := range []string{owner, otherOwner} {
		require.NoError(t, utils.SetUser(context.Background(), utils.User{
			Name: name,
			Rpm:  1000,
			Tpm:  100000,
		}, redisClient))
	}

	var createBody bytes.Buffer
	writer := multipart.NewWriter(&createBody)
	require.NoError(t, writer.WriteField("model", modelName))
	require.NoError(t, writer.WriteField("prompt", "A small robot waves at the camera"))
	require.NoError(t, writer.Close())

	// Deliberately omit routing-strategy. The gateway must inject its default at
	// RequestHeaders, rematch onto ORIGINAL_DST, and send the create to the same
	// pod whose identity it records for every follow-up request below. The mock
	// model has multiple replicas, so comparing MockPod across phases exercises
	// the pin rather than relying on a single-pod deployment.
	resp, payload := doVideoE2ERequest(t, http.MethodPost, "/v1/videos", owner, writer.FormDataContentType(), &createBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(payload))
	var created videoE2EResponse
	require.NoError(t, json.Unmarshal(payload, &created))
	require.True(t, strings.HasPrefix(created.ID, asyncVideoPublicIDPrefix), "gateway returned a backend video id: %s", payload)
	require.Equal(t, modelName, created.Model)
	require.NotEmpty(t, created.MockPod)

	resp, payload = doVideoE2ERequest(t, http.MethodGet, "/v1/videos/"+created.ID, owner, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(payload))
	var status videoE2EResponse
	require.NoError(t, json.Unmarshal(payload, &status))
	assert.Equal(t, created.ID, status.ID)
	assert.Equal(t, created.MockPod, status.MockPod, "follow-up request was not pinned to the creating pod")

	resp, payload = doVideoE2ERequest(t, http.MethodGet, "/v1/videos/"+created.ID+"/content", owner, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(payload))
	assert.Equal(t, "video/mp4", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(payload), created.MockPod)

	resp, payload = doVideoE2ERequest(t, http.MethodGet, "/v1/videos?limit=20&order=desc", owner, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(payload))
	var list videoE2EListResponse
	require.NoError(t, json.Unmarshal(payload, &list))
	require.Len(t, list.Data, 1)
	assert.Equal(t, created.ID, list.Data[0].ID)
	assert.False(t, list.HasMore)

	resp, payload = doVideoE2ERequest(t, http.MethodGet, "/v1/videos/"+created.ID, otherOwner, "", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(payload))

	resp, payload = doVideoE2ERequest(t, http.MethodDelete, "/v1/videos/"+created.ID, owner, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(payload))
	var deleted videoE2EResponse
	require.NoError(t, json.Unmarshal(payload, &deleted))
	assert.Equal(t, created.ID, deleted.ID)
	assert.True(t, deleted.Deleted)

	resp, payload = doVideoE2ERequest(t, http.MethodGet, "/v1/videos/"+created.ID, owner, "", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(payload))
}
