/*
Copyright 2024 The Aibrix Team.

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
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStrategyRequiresCache(t *testing.T) {
	req := "this is test message"
	targetPod := getTargetPodFromChatCompletion(t, req, "least-request")
	assert.NotEmpty(t, targetPod, "least request target pod is empty")
}

func TestRandomRouting(t *testing.T) {
	histogram := make(map[string]int)
	iterration := 100

	for i := 0; i < iterration; i++ {
		req := "hello test"
		targetPod := getTargetPodFromChatCompletion(t, req, "random")
		assert.NotEmpty(t, targetPod, "target pod should not be empty")
		histogram[targetPod]++
	}

	assert.True(t, len(histogram) > 1, "target pod distribution should be more than 1")

	// Calculate the variance of the distribution stored in the histogram using sum and sum of squared values
	sum := float64(iterration)
	var sumSquared float64
	for _, count := range histogram {
		sumSquared += float64(count) * float64(count)
	}
	mean := sum / float64(len(histogram))
	stddev := math.Sqrt(sumSquared/float64(len(histogram)) - mean*mean)

	assert.True(t, stddev/mean < 0.2,
		"standard deviation of pod distribution should be less than 20%%, but got %f, mean %f", stddev, mean)
}

func TestPrefixCacheRouting(t *testing.T) {
	// #1 request - cache first time request
	req := "ensure test message is longer than 128 bytes!! this is first message! 这是测试消息！"
	targetPod := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod)

	// #2 request - reuse target pod from first time
	targetPod2 := getTargetPodFromChatCompletion(t, req, "prefix-cache")
	t.Logf("req: %s, target pod: %v\n", req, targetPod2)
	assert.Equal(t, targetPod, targetPod2)

	// #3 request - new request, match to random pod
	var count int
	for count < 5 {
		generateMessage := fmt.Sprintf("ensure test message is longer than 128 bytes!! this is %v message! 这是测试消息！",
			rand.Intn(1000))
		targetPod3 := getTargetPodFromChatCompletion(t, generateMessage, "prefix-cache")
		t.Logf("req: %s, target pod from #3 request: %v\n", generateMessage, targetPod3)
		if targetPod != targetPod3 {
			break
		}
		count++
	}

	assert.NotEqual(t, 5, count)
}

func getTargetPodFromChatCompletion(t *testing.T, message string, strategy string) string {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, strategy, option.WithResponseInto(&dst))

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		}),
		Model: openai.F(modelName),
	})
	require.NoError(t, err, "chat completitions failed %v", err)
	assert.Equal(t, modelName, chatCompletion.Model)

	return dst.Header.Get("target-pod")
}
