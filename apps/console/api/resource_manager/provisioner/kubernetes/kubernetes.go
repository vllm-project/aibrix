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

package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managerName    = "aibrix-console-provisioner"
)

type Provisioner struct {
	client kubernetes.Interface
}

var _ provisioner.Provisioner = (*Provisioner)(nil)

func New() (*Provisioner, error) {
	cfg, err := buildConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes provisioner: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes provisioner: %w", err)
	}
	return &Provisioner{client: client}, nil
}

func (p *Provisioner) Type() types.ResourceProvisionType {
	return types.ResourceProvisionTypeKubernetes
}

func (p *Provisioner) Provision(ctx context.Context, req *types.ResourceProvision) (*types.ProvisionResult, error) {
	if req == nil {
		return nil, fmt.Errorf("kubernetes provisioner: nil request")
	}

	provisionID := "k8s-" + req.IdempotencyKey
	if req.IdempotencyKey == "" {
		provisionID = fmt.Sprintf("k8s-%d", time.Now().UnixNano())
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      provisionID,
			Namespace: "default",
			Labels: map[string]string{
				labelManagedBy: managerName,
			},
		},
		Data: map[string]string{
			"status":         string(types.ProvisionStatusRunning),
			"idempotencyKey": req.IdempotencyKey,
			"createdAt":      time.Now().UTC().Format(time.RFC3339),
		},
	}

	_, err := p.client.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(2).Infof("[provisioner.kubernetes] idempotent hit: %s", provisionID)
			return &types.ProvisionResult{
				ProvisionID:    provisionID,
				IdempotencyKey: req.IdempotencyKey,
				Status:         types.ProvisionStatusRunning,
			}, nil
		}
		return nil, fmt.Errorf("kubernetes provisioner: create failed: %w", err)
	}

	klog.V(2).Infof("[provisioner.kubernetes] provisioned %s", provisionID)
	now := time.Now().UTC()
	return &types.ProvisionResult{
		ProvisionID:    provisionID,
		IdempotencyKey: req.IdempotencyKey,
		Status:         types.ProvisionStatusRunning,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

func (p *Provisioner) Release(_ context.Context, _ string) error {
	return nil
}

func (p *Provisioner) List(_ context.Context, _ *types.ListOptions) ([]*types.ProvisionResult, error) {
	return nil, nil
}

func buildConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	}
	return cfg, nil
}
