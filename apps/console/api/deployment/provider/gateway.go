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

package provider

import (
	"fmt"

	"github.com/vllm-project/aibrix/apps/console/api/config"
	gatewayprovider "github.com/vllm-project/aibrix/apps/console/api/gateway/provider"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

type gatewayClientProvider struct {
	kubeconfigClientProvider
	client gatewayclient.Interface
}

// NewGatewayClientProvider uses the same Kubernetes configuration as model
// deployments without requiring Gateway API support from existing client fakes.
func NewGatewayClientProvider(cfg config.KubernetesProviderConfig) gatewayprovider.ClientProvider {
	return &gatewayClientProvider{kubeconfigClientProvider: kubeconfigClientProvider{cfg: cfg}}
}

func (p *gatewayClientProvider) GatewayClient() (gatewayclient.Interface, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, p.cachedNamespace, nil
	}
	cfg, namespace, err := p.restConfig()
	if err != nil {
		return nil, "", apierrors.NewServiceUnavailable(fmt.Sprintf("configure Gateway client: %v", err))
	}
	client, err := gatewayclient.NewForConfig(cfg)
	if err != nil {
		return nil, "", apierrors.NewServiceUnavailable(fmt.Sprintf("initialize Gateway client: %v", err))
	}
	p.client = client
	return client, namespace, nil
}
