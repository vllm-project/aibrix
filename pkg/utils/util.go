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
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
	"k8s.io/klog/v2"
)

// https://cookbook.openai.com/examples/how_to_count_tokens_with_tiktoken
const encoding = "cl100k_base"

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
	if err := json.Unmarshal([]byte(message), &messages); err != nil {
		// If array parsing fails, try single message
		var msg Message
		if err := json.Unmarshal([]byte(message), &msg); err != nil {
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
