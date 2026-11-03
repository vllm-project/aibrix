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
	"testing"

	"k8s.io/client-go/rest"
)

func TestGatewayClientCachesNamespace(t *testing.T) {
	for _, namespace := range []string{defaultKubernetesNamespace, "inference"} {
		t.Run(namespace, func(t *testing.T) {
			p := &gatewayClientProvider{
				kubeconfigClientProvider: kubeconfigClientProvider{
					cachedRESTConfig: &rest.Config{Host: "https://kubernetes.example"},
					cachedNamespace:  namespace,
				},
			}
			client, resolvedNamespace, err := p.GatewayClient()
			if err != nil {
				t.Fatal(err)
			}
			if client == nil || resolvedNamespace != namespace {
				t.Fatalf("initial client = %v, namespace = %q, want %q", client, resolvedNamespace, namespace)
			}

			// Once initialized, the Gateway cache must not depend on the
			// embedded provider's configuration cache.
			p.cachedRESTConfig = nil
			p.cachedNamespace = ""
			cachedClient, cachedNamespace, err := p.GatewayClient()
			if err != nil {
				t.Fatal(err)
			}
			if cachedClient != client || cachedNamespace != namespace {
				t.Fatalf("cached client changed or lost namespace: %q, want %q", cachedNamespace, namespace)
			}
		})
	}
}
