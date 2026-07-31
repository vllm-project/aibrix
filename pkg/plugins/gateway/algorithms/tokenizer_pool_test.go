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

package routingalgorithms

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mock tokenizer for testing
type mockTokenizer struct {
	mock.Mock
}

func (m *mockTokenizer) TokenizeInputText(text string) ([]byte, error) {
	args := m.Called(text)
	return args.Get(0).([]byte), args.Error(1)
}

func (m *mockTokenizer) TokenizeWithOptions(ctx context.Context, input tokenizer.TokenizeInput) (*tokenizer.TokenizeResult, error) {
	args := m.Called(ctx, input)
	if result := args.Get(0); result != nil {
		return result.(*tokenizer.TokenizeResult), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *mockTokenizer) Detokenize(ctx context.Context, tokens []int) (string, error) {
	args := m.Called(ctx, tokens)
	return args.String(0), args.Error(1)
}

func (m *mockTokenizer) GetEndpoint() string {
	args := m.Called()
	return args.String(0)
}

func (m *mockTokenizer) IsHealthy(ctx context.Context) bool {
	args := m.Called(ctx)
	return args.Bool(0)
}

func (m *mockTokenizer) Close() error {
	args := m.Called()
	return args.Error(0)
}

// Helper function to create test pods
func createTestPod(name, model, engine string, ready bool) v1.Pod {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.ModelLabelName: model,
			},
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "vllm",
					Env: []v1.EnvVar{
						{Name: "INFERENCE_ENGINE", Value: engine},
					},
					Ports: []v1.ContainerPort{
						{ContainerPort: 8000},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: fmt.Sprintf("10.0.0.%d", len(name)%255),
		},
	}

	if ready {
		pod.Status.Conditions = []v1.PodCondition{
			{Type: v1.PodReady, Status: v1.ConditionTrue},
		}
	}

	return pod
}

func TestGetModelFromPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      v1.Pod
		expected string
	}{
		{
			name: "model from label",
			pod: v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.ModelLabelName: "llama2-7b",
					},
				},
			},
			expected: "llama2-7b",
		},
		{
			name: "model from annotation",
			pod: v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						constants.ModelLabelName: "qwen-7b",
					},
				},
			},
			expected: "qwen-7b",
		},
		{
			name: "model from env var MODEL_NAME",
			pod: v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Env: []v1.EnvVar{
								{Name: "MODEL_NAME", Value: "gpt2"},
							},
						},
					},
				},
			},
			expected: "gpt2",
		},
		{
			name: "model from env var MODEL",
			pod: v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Env: []v1.EnvVar{
								{Name: "MODEL", Value: "bert-base"},
							},
						},
					},
				},
			},
			expected: "bert-base",
		},
		{
			name:     "no model info",
			pod:      v1.Pod{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getModelFromPod(&tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsVLLMPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      v1.Pod
		expected bool
	}{
		{
			name: "vllm from label",
			pod: v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.ModelLabelEngine: "vllm",
					},
				},
			},
			expected: true,
		},
		{
			name: "vllm from annotation",
			pod: v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						constants.ModelLabelEngine: "vllm",
					},
				},
			},
			expected: true,
		},
		{
			name: "vllm from env var",
			pod: v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Env: []v1.EnvVar{
								{Name: "INFERENCE_ENGINE", Value: "vllm"},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "vllm from port 8000",
			pod: v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Ports: []v1.ContainerPort{
								{ContainerPort: 8000},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "not vllm",
			pod: v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.ModelLabelEngine: "sglang",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isVLLMPod(&tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name     string
		pod      v1.Pod
		expected bool
	}{
		{
			name: "ready pod",
			pod: v1.Pod{
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionTrue},
					},
				},
			},
			expected: true,
		},
		{
			name: "not running",
			pod: v1.Pod{
				Status: v1.PodStatus{
					Phase: v1.PodPending,
				},
			},
			expected: false,
		},
		{
			name: "running but not ready",
			pod: v1.Pod{
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					Conditions: []v1.PodCondition{
						{Type: v1.PodReady, Status: v1.ConditionFalse},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPodReady(&tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// createTestCache creates a test cache that doesn't require initialization
func createTestCache() cache.Cache {
	// Create a new test cache instance
	testCache := cache.NewForTest()
	return testCache
}

// resetPrometheusRegistry resets the Prometheus registry to avoid duplicate registration
func resetPrometheusRegistry() {
	// Create a new registry for each test to avoid conflicts
	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg
	prometheus.DefaultGatherer = reg
}

func TestTokenizerPoolGetTokenizer(t *testing.T) {
	resetPrometheusRegistry()

	// Create a mock default tokenizer
	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	// Create test cache
	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     false, // Disabled initially
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    30 * time.Second,
		TokenizerTTL:         5 * time.Minute,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
		Timeout:              10 * time.Second,
		ModelServiceMap:      map[string]string{},
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	t.Run("remote disabled returns default", func(t *testing.T) {
		pod := createTestPod("pod1", "llama2-7b", "vllm", true)
		pods := []*v1.Pod{&pod}

		tok := pool.GetTokenizer("llama2-7b", pods)
		assert.Equal(t, defaultTokenizer, tok)
	})

	t.Run("no matching pod returns default", func(t *testing.T) {
		pool.config.EnableVLLMRemote = true

		pod := createTestPod("pod1", "different-model", "vllm", true)
		pods := []*v1.Pod{&pod}

		tok := pool.GetTokenizer("llama2-7b", pods)
		assert.Equal(t, defaultTokenizer, tok)
	})

	t.Run("service map takes precedence", func(t *testing.T) {
		pool.config.EnableVLLMRemote = true
		pool.config.ModelServiceMap["llama2-7b"] = "http://llama-service:8000"

		pod := createTestPod("pod1", "llama2-7b", "vllm", true)
		pods := []*v1.Pod{&pod}

		// Since we can't create actual remote tokenizers in unit tests,
		// just verify the endpoint discovery works
		endpoint := pool.findVLLMEndpointForModel("llama2-7b", pods)
		assert.Equal(t, "http://llama-service:8000", endpoint)
	})
}

func TestTokenizerPoolHealthCheck(t *testing.T) {
	resetPrometheusRegistry()

	// Create mock tokenizers
	healthyTokenizer := &mockTokenizer{}
	healthyTokenizer.On("IsHealthy", mock.Anything).Return(true)
	healthyTokenizer.On("Close").Return(nil)

	unhealthyTokenizer := &mockTokenizer{}
	unhealthyTokenizer.On("IsHealthy", mock.Anything).Return(false)
	unhealthyTokenizer.On("Close").Return(nil)

	defaultTokenizer := &mockTokenizer{}

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		HealthCheckPeriod:    100 * time.Millisecond,
		TokenizerTTL:         200 * time.Millisecond,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	// Manually add tokenizers to the pool
	pool.mu.Lock()
	pool.tokenizers["model1"] = &tokenizerEntry{
		tokenizer:    healthyTokenizer,
		endpoint:     "http://10.0.0.1:8000",
		lastUsed:     time.Now(),
		lastHealthy:  time.Now(),
		healthStatus: true,
	}
	pool.tokenizers["model2"] = &tokenizerEntry{
		tokenizer:    unhealthyTokenizer,
		endpoint:     "http://10.0.0.2:8000",
		lastUsed:     time.Now(),
		lastHealthy:  time.Now(),
		healthStatus: true,
	}
	pool.mu.Unlock()

	// Let health checker run
	time.Sleep(150 * time.Millisecond)

	// Check health statuses
	pool.mu.RLock()
	assert.True(t, pool.tokenizers["model1"].healthStatus)
	assert.False(t, pool.tokenizers["model2"].healthStatus)
	pool.mu.RUnlock()

	// Wait for TTL to expire
	time.Sleep(250 * time.Millisecond)

	// Check that stale tokenizers are removed
	pool.mu.RLock()
	assert.Len(t, pool.tokenizers, 0)
	pool.mu.RUnlock()

	healthyTokenizer.AssertExpectations(t)
	unhealthyTokenizer.AssertExpectations(t)
}

func TestTokenizerPoolMaxSize(t *testing.T) {
	resetPrometheusRegistry()

	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		MaxTokenizersPerPool: 2,
		DefaultTokenizer:     defaultTokenizer,
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    30 * time.Second,
		TokenizerTTL:         5 * time.Minute,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	// Create mock tokenizers with Close expectations
	mockTok1 := &mockTokenizer{}
	mockTok1.On("Close").Return(nil)
	mockTok2 := &mockTokenizer{}
	mockTok2.On("Close").Return(nil)

	// Fill the pool to max capacity
	pool.mu.Lock()
	pool.tokenizers["model1"] = &tokenizerEntry{tokenizer: mockTok1, healthStatus: true}
	pool.tokenizers["model2"] = &tokenizerEntry{tokenizer: mockTok2, healthStatus: true}
	pool.mu.Unlock()

	// Try to get a tokenizer for a new model
	pod := createTestPod("pod3", "model3", "vllm", true)
	pods := []*v1.Pod{&pod}

	tok := pool.GetTokenizer("model3", pods)
	assert.Equal(t, defaultTokenizer, tok)

	// Verify pool size didn't increase
	pool.mu.RLock()
	assert.Len(t, pool.tokenizers, 2)
	pool.mu.RUnlock()
}

func TestTokenizerPoolConcurrency(t *testing.T) {
	resetPrometheusRegistry()

	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    100 * time.Millisecond,
		TokenizerTTL:         200 * time.Millisecond,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
		Timeout:              1 * time.Second,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	// Simulate concurrent access
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tok := pool.GetTokenizer("test-model", nil)
				// Simulate usage
				time.Sleep(time.Microsecond)
				_ = tok
			}
		}(i)
	}

	// Wait for all goroutines
	wg.Wait()
}

func TestTokenizerPoolRaceCondition(t *testing.T) {
	resetPrometheusRegistry()

	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    10 * time.Millisecond,
		TokenizerTTL:         50 * time.Millisecond,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
		Timeout:              1 * time.Second,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	// Create a mock pod
	pod := createTestPod("test-pod", "test-model", "vllm", true)
	pods := []*v1.Pod{&pod}

	// Pre-populate the tokenizer
	mockTok := &mockTokenizer{}
	mockTok.On("IsHealthy", mock.Anything).Return(true)
	mockTok.On("Close").Return(nil).Maybe()

	pool.mu.Lock()
	pool.tokenizers["test-model"] = &tokenizerEntry{
		tokenizer:    mockTok,
		endpoint:     "http://10.0.0.1:8000",
		lastUsed:     time.Now(),
		lastHealthy:  time.Now(),
		healthStatus: true,
	}
	pool.mu.Unlock()

	// Start goroutines that continuously access the tokenizer
	done := make(chan bool)
	var wg sync.WaitGroup

	// Reader goroutines
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					tok := pool.GetTokenizer("test-model", pods)
					_ = tok
					time.Sleep(time.Microsecond)
				}
			}
		}()
	}

	// Let it run for a while to trigger cleanup
	time.Sleep(100 * time.Millisecond)

	// Signal all goroutines to stop
	close(done)
	wg.Wait()
}

func TestCreateOrUpdateConcurrency(t *testing.T) {
	resetPrometheusRegistry()

	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    30 * time.Second,
		TokenizerTTL:         5 * time.Minute,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
		Timeout:              10 * time.Second,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("Failed to close pool: %v", err)
		}
	}()

	// Create test pods
	pods := make([]*v1.Pod, 5)
	for i := 0; i < 5; i++ {
		pod := createTestPod(fmt.Sprintf("pod%d", i), fmt.Sprintf("model%d", i), "vllm", true)
		pods[i] = &pod
	}

	// Test concurrent creation of tokenizers for different models
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(modelIdx int) {
			defer wg.Done()
			model := fmt.Sprintf("model%d", modelIdx)

			// Multiple goroutines try to create tokenizer for same model
			var innerWg sync.WaitGroup
			for j := 0; j < 3; j++ {
				innerWg.Add(1)
				go func() {
					defer innerWg.Done()
					tok := pool.GetTokenizer(model, pods)
					_ = tok
				}()
			}
			innerWg.Wait()
		}(i)
	}
	wg.Wait()

	// Verify no deadlocks or race conditions occurred
	// The test passing with -race flag is the main validation
}

func BenchmarkTokenizerPoolConcurrentCreation(b *testing.B) {
	resetPrometheusRegistry()

	defaultTokenizer := &mockTokenizer{}
	defaultTokenizer.On("TokenizeInputText", mock.Anything).Return([]byte("default"), nil)

	testCache := createTestCache()

	config := TokenizerPoolConfig{
		EnableVLLMRemote:     true,
		EndpointTemplate:     "http://%s:8000",
		HealthCheckPeriod:    30 * time.Second,
		TokenizerTTL:         5 * time.Minute,
		MaxTokenizersPerPool: 100,
		DefaultTokenizer:     defaultTokenizer,
		Timeout:              10 * time.Second,
	}

	pool := NewTokenizerPool(config, testCache)
	defer func() {
		if err := pool.Close(); err != nil {
			b.Errorf("Failed to close pool: %v", err)
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tok := pool.GetTokenizer("test-model", nil)
			_ = tok
		}
	})
}

func BenchmarkTimeNowOptimization(b *testing.B) {
	// Unoptimized version
	b.Run("Unoptimized", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = &tokenizerEntry{
				lastUsed:     time.Now(),
				lastHealthy:  time.Now(),
				healthStatus: true,
			}
		}
	})

	// Optimized version
	b.Run("Optimized", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			now := time.Now()
			_ = &tokenizerEntry{
				lastUsed:     now,
				lastHealthy:  now,
				healthStatus: true,
			}
		}
	})
}
