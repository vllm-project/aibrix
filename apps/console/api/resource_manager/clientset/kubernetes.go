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

package clientset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"k8s.io/klog/v2"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// NewK8sClientset creates a Kubernetes clientset from a KubernetesCredential.
// This is the main entry point for creating a clientset.
func NewK8sClientset(credential *types.KubernetesCredential) (*types.KubernetesClientset, error) {
	if credential == nil {
		return nil, types.ErrCredentialIsNil
	}

	inCluster, err := resolveCredential(credential)
	if err != nil {
		return nil, err
	}

	if inCluster {
		return newInClusterClientset(credential)
	}

	// Validate kubeconfig and parameters before creating clientset
	rawConfig, err := validateCredential(credential)
	if err != nil {
		return nil, err
	}

	return newClientsetFromKubeconfig(credential, rawConfig)
}

// resolveCredential resolves a KubernetesCredential.
// It sets default values for missing fields and determines the authentication method.
func resolveCredential(credential *types.KubernetesCredential) (inCluster bool, err error) {
	// If kubeconfig path is explicitly set, use it
	if credential.Kubeconfig != nil && *credential.Kubeconfig != "" {
		return false, nil
	}

	// If ServiceAccountName is set, prefer in-cluster auth
	if credential.ServiceAccountName != nil && *credential.ServiceAccountName != "" {
		return true, nil
	}

	// Try default locations
	return resolveDefaultCredential(credential)
}

// resolveDefaultCredential tries to find kubeconfig from default locations.
func resolveDefaultCredential(credential *types.KubernetesCredential) (inCluster bool, err error) {
	// Try KUBECONFIG env first
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		credential.Kubeconfig = &kubeconfig
		return false, nil
	}

	// Try ~/.kube/config
	home, err := os.UserHomeDir()
	if err == nil {
		defaultPath := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(defaultPath); err == nil {
			credential.Kubeconfig = &defaultPath
			return false, nil
		}
	}

	// Fall back to in-cluster config
	return true, nil
}

// validateCredential validates the kubeconfig file and parameters.
// It checks:
// 1. Kubeconfig file exists and is readable
// 2. If context is specified, it exists in the kubeconfig
// 3. If namespace is specified, it's a valid namespace name format
func validateCredential(credential *types.KubernetesCredential) (*clientcmdapi.Config, error) {
	if credential.Kubeconfig == nil || *credential.Kubeconfig == "" {
		return nil, types.ErrInvalidCredential
	}

	// Check kubeconfig file exists
	if _, err := os.Stat(*credential.Kubeconfig); err != nil {
		klog.Errorf("kubeconfig file not found: %s", *credential.Kubeconfig)
		return nil, types.ErrInvalidCredential
	}

	// Load kubeconfig to validate context
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = *credential.Kubeconfig
	rawConfig, err := loadingRules.Load()
	if err != nil {
		klog.Errorf("failed to load kubeconfig: %s", err.Error())
		return nil, types.ErrInvalidCredential
	}

	// Validate context if specified
	if credential.Context != nil && *credential.Context != "" {
		if _, exists := rawConfig.Contexts[*credential.Context]; !exists {
			klog.Errorf("context %q not found in kubeconfig", *credential.Context)
			return nil, types.ErrInvalidCredential
		}
	} else if len(rawConfig.Contexts) == 0 {
		klog.Error("no contexts found in kubeconfig")
		return nil, types.ErrInvalidCredential
	} else if rawConfig.CurrentContext == "" && len(rawConfig.Contexts) == 1 {
		// Single context but no current-context set is fine
	} else if rawConfig.CurrentContext == "" {
		klog.Error("no current-context set in kubeconfig and no context specified")
		return nil, types.ErrInvalidCredential
	}

	return rawConfig, nil
}

// newClientsetFromKubeconfig creates clientsets using kubeconfig file.
// Assumes validateCredential has already been called.
func newClientsetFromKubeconfig(credential *types.KubernetesCredential, rawConfig *clientcmdapi.Config) (*types.KubernetesClientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = *credential.Kubeconfig

	regions := buildKubeconfigRegions(rawConfig, credential)
	if len(regions) == 0 {
		return nil, types.ErrInvalidCredential
	}

	regionClients := make([]types.KubernetesRegionClient, 0, len(regions))
	for _, region := range regions {
		if region.Kubernetes == nil {
			continue
		}

		configOverrides := &clientcmd.ConfigOverrides{
			CurrentContext: region.Kubernetes.Context,
			Context: clientcmdapi.Context{
				Namespace: region.Kubernetes.Namespace,
			},
		}
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

		restConfig, err := clientConfig.ClientConfig()
		if err != nil {
			klog.Errorf("build rest config failed for context %q namespace %q: %s", region.Kubernetes.Context, region.Kubernetes.Namespace, err.Error())
			return nil, types.ErrInvalidCredential
		}

		regionCopy := region
		if regionCopy.Kubernetes != nil && regionCopy.Kubernetes.Cluster == "" {
			regionCopy.Kubernetes.Cluster = restConfig.Host
		}

		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			klog.Errorf("create clientset failed for context %q namespace %q: %s", region.Kubernetes.Context, region.Kubernetes.Namespace, err.Error())
			return nil, types.ErrInvalidCredential
		}

		regionClients = append(regionClients, types.KubernetesRegionClient{
			Region:    regionCopy,
			Clientset: clientset,
		})
	}

	if len(regionClients) == 0 {
		return nil, types.ErrInvalidCredential
	}

	return &types.KubernetesClientset{
		RegionClients: regionClients,
	}, nil
}

// newInClusterClientset creates a clientset using in-cluster config.
func newInClusterClientset(credential *types.KubernetesCredential) (*types.KubernetesClientset, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Errorf("get in-cluster config failed: %s", err.Error())
		return nil, types.ErrInvalidCredential
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Errorf("create in-cluster clientset failed: %s", err.Error())
		return nil, types.ErrInvalidCredential
	}

	namespace := getStringValue(credential.Namespace)
	if namespace == "" {
		namespace = "default"
	}

	return &types.KubernetesClientset{
		RegionClients: []types.KubernetesRegionClient{
			{
				Region: types.RegionSpec{
					Kubernetes: &types.KubernetesRegion{
						Cluster:   restConfig.Host,
						Namespace: namespace,
					},
				},
				Clientset: clientset,
			},
		},
	}, nil
}

func buildKubeconfigRegions(rawConfig *clientcmdapi.Config, credential *types.KubernetesCredential) []types.RegionSpec {
	namespaceOverride := getStringValue(credential.Namespace)
	contextOverride := getStringValue(credential.Context)

	selectedContexts := make(map[string]*clientcmdapi.Context)
	if contextOverride != "" {
		if ctx, ok := rawConfig.Contexts[contextOverride]; ok {
			selectedContexts[contextOverride] = ctx
		}
	} else {
		for name, ctx := range rawConfig.Contexts {
			selectedContexts[name] = ctx
		}
	}

	if len(selectedContexts) == 0 {
		return nil
	}

	keys := make([]string, 0, len(selectedContexts))
	for contextName := range selectedContexts {
		keys = append(keys, contextName)
	}
	sort.Strings(keys)

	regions := make([]types.RegionSpec, 0, len(keys))
	seen := make(map[string]struct{})
	for _, contextName := range keys {
		ctx := selectedContexts[contextName]
		if ctx == nil {
			continue
		}

		namespace := namespaceOverride
		if namespace == "" {
			namespace = ctx.Namespace
		}
		if namespace == "" {
			namespace = "default"
		}

		cluster := ""
		if rawCluster, ok := rawConfig.Clusters[ctx.Cluster]; ok && rawCluster != nil {
			cluster = rawCluster.Server
		}

		regionKey := contextName + "|" + namespace + "|" + cluster
		if _, ok := seen[regionKey]; ok {
			continue
		}
		seen[regionKey] = struct{}{}

		if cluster == "" {
			if rawCluster, ok := rawConfig.Clusters[ctx.Cluster]; ok && rawCluster != nil {
				cluster = rawCluster.Server
			}
		}
		if cluster == "" {
			cluster = fmt.Sprintf("cluster:%s", ctx.Cluster)
		}

		regions = append(regions, types.RegionSpec{
			Kubernetes: &types.KubernetesRegion{
				Context:   contextName,
				Cluster:   cluster,
				Namespace: namespace,
			},
		})
	}

	return regions
}

// getStringValue safely gets the string value from a pointer.
func getStringValue(ptr *string) string {
	if ptr != nil {
		return *ptr
	}
	return ""
}
