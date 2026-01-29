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

package discovery

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

// EndpointConfig represents a single endpoint in the config file.
type EndpointConfig struct {
	// Address is the endpoint address in "host:port" format.
	Address string `json:"address"`
	// Labels are optional labels for routing (e.g., role-name, roleset-name for P/D).
	Labels map[string]string `json:"labels,omitempty"`
}

// ModelConfig represents a model and its endpoints.
type ModelConfig struct {
	// Name is the model name.
	Name string `json:"name"`
	// Endpoints are the backend endpoints serving this model.
	Endpoints []EndpointConfig `json:"endpoints"`
}

// FileConfig represents the complete endpoints configuration file.
type FileConfig struct {
	// Models is the list of models and their endpoints.
	Models []ModelConfig `json:"models"`
}

// FileProvider implements Provider using a YAML configuration file.
type FileProvider struct {
	configPath string
}

// AddModelAdapterEventHandler implements Provider.
func (p *FileProvider) AddEventHandler(_ string,
	_ cache.ResourceEventHandlerFuncs, _ <-chan struct{}) error {
	return nil
}

// NewFileProvider creates a new file-based discovery provider.
func NewFileProvider(configPath string) *FileProvider {
	return &FileProvider{
		configPath: configPath,
	}
}

// Type returns the provider type identifier.
func (p *FileProvider) Type() string {
	return "file"
}

// Load reads the config file and returns all endpoints as synthetic pods.
func (p *FileProvider) Load() ([]any, error) {
	data, err := os.ReadFile(p.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config FileConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	var pods []any
	for _, model := range config.Models {
		for i, ep := range model.Endpoints {
			pod, err := endpointToPod(model.Name, i, ep)
			if err != nil {
				return nil, err
			}
			pods = append(pods, pod)
		}
	}

	return pods, nil
}

const standaloneNamespace = "standalone"

func endpointToPod(modelName string, index int, ep EndpointConfig) (*v1.Pod, error) {
	// Parse host:port
	host, portStr, err := net.SplitHostPort(ep.Address)
	if err != nil {
		// Assume it's just a host without port
		host = ep.Address
		portStr = "8000"
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q for address %q: %w", portStr, ep.Address, err)
	}

	// Generate pod name from model and address
	safeName := strings.ReplaceAll(host, ".", "-")
	podName := fmt.Sprintf("%s-%s-%d", sanitizeName(modelName), safeName, index)

	// Build labels with required fields
	labels := map[string]string{
		constants.ModelLabelName: modelName,
		constants.ModelLabelPort: portStr,
	}
	// Copy custom labels (e.g., role-name, roleset-name for P/D routing)
	for k, v := range ep.Labels {
		labels[k] = v
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: standaloneNamespace,
			Labels:    labels,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: host,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "vllm",
					Ports: []v1.ContainerPort{
						{
							ContainerPort: int32(port),
						},
					},
				},
			},
		},
	}, nil
}

// sanitizeName converts a string to a valid Kubernetes name.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.Trim(name, "-")
	if len(name) > 50 {
		name = name[:50]
	}
	name = strings.TrimRight(name, "-")
	return name
}

var _ Provider = (*FileProvider)(nil)
