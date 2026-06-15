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
	"sigs.k8s.io/yaml"
)

const standaloneNamespace = "standalone"

// RoleSetConfig defines a group of prefill and decode workers that can be paired together.
// The PD routing algorithm scores prefill and decode pods within the same roleset
// to find the optimal pair (e.g., Single node P/D etc).
type RoleSetConfig struct {
	// Name identifies this roleset (used as pairing key in PD routing).
	Name string `json:"name"`
	// Prefill lists prefill worker addresses in "host:port" format.
	Prefill []string `json:"prefill"`
	// Decode lists decode worker addresses in "host:port" format.
	Decode []string `json:"decode"`
}

// StaticModelConfig represents a model and its backend workers.
type StaticModelConfig struct {
	// Name is the model name (e.g., "Qwen/Qwen2.5-72B").
	Name string `json:"name"`
	// Engine is the inference engine type (e.g., "vllm", "sglang", "trtllm"). Optional.
	Engine string `json:"engine,omitempty"`
	// Endpoints lists worker addresses for non-disaggregated serving.
	// Each entry is a "host:port" string. Mutually exclusive with RoleSets.
	Endpoints []string `json:"endpoints,omitempty"`
	// RoleSets defines prefill/decode worker groups for disaggregated serving.
	// Mutually exclusive with Endpoints.
	RoleSets []RoleSetConfig `json:"rolesets,omitempty"`
}

// StaticConfig represents the complete static endpoints configuration.
type StaticConfig struct {
	// Models is the list of models and their workers.
	Models []StaticModelConfig `json:"models"`
}

// StaticProvider implements Provider by loading a static YAML configuration file.
// The configuration is loaded once at startup; no dynamic updates are supported.
type StaticProvider struct {
	configPath string
}

// NewStaticProvider creates a new static discovery provider.
func NewStaticProvider(configPath string) *StaticProvider {
	return &StaticProvider{
		configPath: configPath,
	}
}

// Type returns the provider type identifier.
func (p *StaticProvider) Type() string {
	return "static"
}

// Watch reads the config file, delivers all endpoints as EventAdd via the handler,
// and returns. Static provider has no ongoing dynamic updates.
func (p *StaticProvider) Watch(handler EventHandler, _ <-chan struct{}) error {
	pods, err := p.load()
	if err != nil {
		return err
	}
	for _, pod := range pods {
		handler(WatchEvent{Type: EventAdd, Object: pod})
	}
	return nil
}

// load reads the config file and returns all endpoints as synthetic pods.
func (p *StaticProvider) load() ([]any, error) {
	data, err := os.ReadFile(p.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config StaticConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	var pods []any
	idx := 0

	for _, model := range config.Models {
		if len(model.Endpoints) > 0 && len(model.RoleSets) > 0 {
			return nil, fmt.Errorf(
				"model %q: endpoints and rolesets are mutually exclusive", model.Name)
		}

		// Non-disaggregated: plain endpoints
		for _, addr := range model.Endpoints {
			pod, err := addressToPod(model.Name, model.Engine, nil, idx, addr)
			if err != nil {
				return nil, fmt.Errorf("model %q endpoint %q: %w", model.Name, addr, err)
			}
			pods = append(pods, pod)
			idx++
		}

		// Disaggregated: rolesets with prefill/decode
		for _, rs := range model.RoleSets {
			if rs.Name == "" {
				return nil, fmt.Errorf("model %q: roleset name is required", model.Name)
			}
			roles := []struct {
				name  string
				addrs []string
			}{
				{"prefill", rs.Prefill},
				{"decode", rs.Decode},
			}
			for _, role := range roles {
				for _, addr := range role.addrs {
					labels := map[string]string{
						"role-name":    role.name,
						"roleset-name": rs.Name,
					}
					pod, err := addressToPod(model.Name, model.Engine, labels, idx, addr)
					if err != nil {
						return nil, fmt.Errorf(
							"model %q roleset %q %s %q: %w",
							model.Name, rs.Name, role.name, addr, err)
					}
					pods = append(pods, pod)
					idx++
				}
			}
		}
	}

	return pods, nil
}

// addressToPod creates a synthetic v1.Pod from an address string and optional labels.
func addressToPod(
	modelName, engine string, extraLabels map[string]string, index int, address string,
) (*v1.Pod, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		// Assume it's just a host without port
		host = address
		portStr = "8000"
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port %q: %w", portStr, err)
	}

	safeName := strings.ReplaceAll(host, ".", "-")
	podName := fmt.Sprintf("%s-%s-%d", sanitizeName(modelName), safeName, index)

	labels := map[string]string{
		constants.ModelLabelName: modelName,
		constants.ModelLabelPort: portStr,
	}
	if engine != "" {
		labels[constants.ModelLabelEngine] = engine
	}
	for k, v := range extraLabels {
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

var _ Provider = (*StaticProvider)(nil)
