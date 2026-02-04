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

package utils

import (
	crand "crypto/rand"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/bytedance/sonic"
	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// https://cookbook.openai.com/examples/how_to_count_tokens_with_tiktoken
const (
	encoding         = "cl100k_base"
	DefaultLLMEngine = "vllm"
)

var tke *tiktoken.Tiktoken

func init() {
	// Tiktoken initialization is slow, so we can init it once and use it in the function
	// if you don't want download dictionary at runtime, you can use offline loader
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	var err error
	tke, err = tiktoken.GetEncoding(encoding)
	if err != nil {
		panic(err)
	}
}

func TokenizeInputText(text string) ([]int, error) {
	// encode
	token := tke.Encode(text, nil, nil)
	return token, nil
}

func DetokenizeText(tokenIds []int) (string, error) {
	decoded := tke.Decode(tokenIds)
	return decoded, nil
}

type Message struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

func TrimMessage(message string) string {
	var messages []Message
	if err := sonic.Unmarshal([]byte(message), &messages); err != nil {
		// If array parsing fails, try single message
		var msg Message
		if err := sonic.Unmarshal([]byte(message), &msg); err != nil {
			return message
		}
		return msg.Content
	}
	if len(messages) > 0 {
		return messages[0].Content
	}
	return message
}

// LookupEnv retrieves an environment variable and returns whether it exists.
// It returns the value and a boolean indicating its existence.
func LookupEnv(key string) (string, bool) {
	value, exists := os.LookupEnv(key)
	return value, exists
}

// LoadEnv loads an environment variable or returns a default value if not set.
func LoadEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Warningf("environment variable %s is not set, using default value: %s", key, defaultValue)
		return defaultValue
	}
	return value
}

func LoadEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Warningf("invalid %s: %s, falling back to default: %d", key, value, defaultValue)
		} else {
			klog.Infof("set %s: %d", key, intValue)
			return intValue
		}
	}
	klog.Infof("set %s: %d, using default value", key, defaultValue)
	return defaultValue
}

func LoadEnvFloat(key string, defaultValue float64) float64 {
	valueStr := os.Getenv(key)
	if valueStr != "" {
		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil || value <= 0 {
			klog.Warningf("invalid %s: %s, falling back to default: %g", key, valueStr, defaultValue)
		} else {
			klog.Infof("set %s: %g", key, value)
			return value
		}
	}
	klog.Infof("set %s: %g, using default value", key, defaultValue)
	return defaultValue
}

// LoadEnvBool loads a boolean environment variable or returns a default value if not set.
func LoadEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value != "" {
		boolValue, err := strconv.ParseBool(value)
		if err != nil {
			klog.Warningf("invalid %s: %s, falling back to default: %v", key, value, defaultValue)
		} else {
			klog.Infof("set %s: %v", key, boolValue)
			return boolValue
		}
	}
	klog.Infof("set %s: %v, using default value", key, defaultValue)
	return defaultValue
}

// LoadEnvDuration loads a duration environment variable or returns a default value if not set.
func LoadEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			klog.Warningf("invalid %s: %s, falling back to default: %v", key, value, defaultValue)
		} else {
			klog.Infof("set %s: %v", key, duration)
			return duration
		}
	}
	klog.Infof("set %s: %v, using default value", key, defaultValue)
	return defaultValue
}

func GetLLMEngine(pod *v1.Pod, labelName string, defaultValue string) string {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		return defaultValue
	}
	return labelTarget
}

// GetPortsForPod returns all ports for a pod based on its configuration.
// For distributed data-parallel (DP) inference, a pod may expose multiple ports:
// - Base port from pod labels (e.g., "model-port")
// - Additional ports for each DP rank (base_port + rank_index)
// Returns nil if the pod doesn't have the required configuration.
func GetPortsForPod(pod *v1.Pod) []int {
	if pod == nil || pod.Labels == nil {
		return nil
	}

	// Get base port from pod labels
	basePort, err := getBasePortFromLabels(pod)
	if err != nil {
		return nil
	}

	// Get data-parallel size from environment variables
	dpSize := getDataParallelSize(pod)
	if dpSize <= 1 {
		// If no DP size, return only the base port
		return []int{basePort}
	}

	// Generate ports for each DP rank: basePort, basePort+1, ..., basePort+(dpSize-1)
	ports := make([]int, dpSize)
	for i := 0; i < dpSize; i++ {
		ports[i] = basePort + i
	}

	return ports
}

// getBasePortFromLabels extracts the base port from pod labels
func getBasePortFromLabels(pod *v1.Pod) (int, error) {
	portStr, ok := pod.Labels[constants.ModelLabelPort]
	if !ok || portStr == "" {
		return 0, fmt.Errorf("no %s label found", constants.ModelLabelPort)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		klog.Warningf("Invalid port value in label %s=%s: %v",
			constants.ModelLabelPort, portStr, err)
		return 0, fmt.Errorf("invalid port format: %w", err)
	}

	if port <= 0 || port > 65535 {
		klog.Warningf("Port %d from label %s is out of valid range",
			port, constants.ModelLabelPort)
		return 0, fmt.Errorf("port %d out of range [1-65535]", port)
	}

	return port, nil
}

// getDataParallelSize retrieves the data parallelism degree from the pod's environment variables.
// This feature requires the user to explicitly pass the "data-parallel-size"
// variable in the pod template's container env section.
// Example:
//
//		  env:
//		  - name: "data-parallel-size"
//	     value: "2"
func getDataParallelSize(pod *v1.Pod) int {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "data-parallel-size" && env.Value != "" {
				size, err := strconv.Atoi(env.Value)
				if err != nil {
					klog.Warningf("Invalid data-parallel-size value '%s': %v",
						env.Value, err)
					return 0
				}

				if size < 0 {
					klog.Warningf("Invalid data-parallel-size %d (must be >= 0)", size)
					return 0
				}

				return size
			}
		}
	}

	return 0
}

// GVKCheckExists check if gvk is support
func GVKCheckExists(cfg *rest.Config, gvk schema.GroupVersionKind) (bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false, err
	}

	// get all resource from GroupVersion
	apiResources, err := dc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			// Group not exist
			return false, nil
		}
		return false, err
	}

	for _, r := range apiResources.APIResources {
		if r.Kind == gvk.Kind {
			return true, nil
		}
	}

	return false, nil
}

// IsDataParallelPod checks if the pod is configured for Distributed Data Parallel.
// It returns true if "data-parallel-size" environment variable is present and its value is greater than 1.
func IsDataParallelPod(pod *v1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "data-parallel-size" && env.Value != "" {
				value, err := strconv.Atoi(env.Value)
				if err == nil && value > 1 {
					return true
				}
			}
		}
	}
	return false
}

func CryptoShuffle[T any](slice []T) {
	n := len(slice)
	for i := n - 1; i > 0; i-- {
		jBig, _ := crand.Int(crand.Reader, big.NewInt(int64(i+1)))
		j := int(jBig.Int64())
		slice[i], slice[j] = slice[j], slice[i]
	}
}
