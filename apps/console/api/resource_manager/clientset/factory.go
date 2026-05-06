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
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

func NewClientset(credential *types.ResourceCredential) (*types.ResourceClientset, error) {
	if credential == nil {
		return nil, types.ErrCredentialIsNil
	}

	clientset := &types.ResourceClientset{
		Provider:   credential.Provider,
		Credential: credential,
	}

	switch credential.Provider {
	case types.ResourceProvisionTypeKubernetes:
		k8sCredential := credential.Kubernetes
		if k8sCredential == nil {
			k8sCredential = &types.KubernetesCredential{}
		}
		k8s, err := NewK8sClientset(k8sCredential)
		if err != nil {
			return nil, err
		}
		clientset.Kubernetes = k8s
	default:
		return nil, types.ErrUnsupportedProvisioner
	}

	return clientset, nil
}
