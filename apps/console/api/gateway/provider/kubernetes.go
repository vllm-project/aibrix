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
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vllm-project/aibrix/apps/console/api/gateway/contract"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewaytyped "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1"
)

// ClientProvider resolves one cluster and namespace from operator configuration.
type ClientProvider interface {
	GatewayClient() (gatewayclient.Interface, string, error)
}

type Kubernetes struct {
	clients ClientProvider
}

var _ contract.Provider = (*Kubernetes)(nil)

func NewKubernetes(clients ClientProvider) *Kubernetes {
	return &Kubernetes{clients: clients}
}

func (p *Kubernetes) client() (gatewaytyped.GatewayInterface, string, error) {
	if p.clients == nil {
		return nil, "", apierrors.NewServiceUnavailable("Gateway client is not configured")
	}
	client, namespace, err := p.clients.GatewayClient()
	if err != nil {
		return nil, "", err
	}
	// An empty namespace would turn List into an all-namespaces operation.
	if client == nil || namespace == "" {
		return nil, "", apierrors.NewServiceUnavailable("Gateway client and namespace are required")
	}
	return client.GatewayV1().Gateways(namespace), namespace, nil
}

func (p *Kubernetes) List(ctx context.Context) (*gatewayv1.GatewayList, error) {
	client, _, err := p.client()
	if err != nil {
		return nil, err
	}
	result, err := client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Name < result.Items[j].Name })
	return result, nil
}

func (p *Kubernetes) Get(ctx context.Context, name string) (*gatewayv1.Gateway, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	client, _, err := p.client()
	if err != nil {
		return nil, err
	}
	return client.Get(ctx, name, metav1.GetOptions{})
}

func (p *Kubernetes) Create(ctx context.Context, request *gatewayv1.Gateway) (*gatewayv1.Gateway, error) {
	client, namespace, err := p.client()
	if err != nil {
		return nil, err
	}
	if err := validateRequest(request, namespace); err != nil {
		return nil, err
	}
	if request.ResourceVersion != "" {
		return nil, apierrors.NewBadRequest("resourceVersion must be empty on create")
	}
	// Only user-owned metadata and spec are writable. Admission and the Gateway
	// controller remain responsible for validation, defaulting and status.
	desired := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name: request.Name, Namespace: namespace,
			Labels: request.Labels, Annotations: request.Annotations,
		},
		Spec: *request.Spec.DeepCopy(),
	}
	return client.Create(ctx, desired, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
}

func (p *Kubernetes) Update(ctx context.Context, request *gatewayv1.Gateway) (*gatewayv1.Gateway, error) {
	client, namespace, err := p.client()
	if err != nil {
		return nil, err
	}
	if err := validateRequest(request, namespace); err != nil {
		return nil, err
	}
	if request.ResourceVersion == "" {
		return nil, apierrors.NewBadRequest("resourceVersion is required on update")
	}
	current, err := client.Get(ctx, request.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != request.ResourceVersion {
		return nil, apierrors.NewConflict(
			schema.GroupResource{Group: gatewayv1.GroupName, Resource: "gateways"},
			request.Name, fmt.Errorf("resourceVersion changed; get the Gateway and retry"),
		)
	}
	current.Spec = *request.Spec.DeepCopy()
	current.Labels = request.Labels
	current.Annotations = request.Annotations
	// Preserve UID, finalizers, owner references and controller-owned status.
	// The API server also checks resourceVersion, closing the Get/Update race.
	return client.Update(ctx, current, metav1.UpdateOptions{FieldValidation: metav1.FieldValidationStrict})
}

func validateRequest(request *gatewayv1.Gateway, namespace string) error {
	if request == nil {
		return apierrors.NewBadRequest("Gateway is required")
	}
	if err := validateName(request.Name); err != nil {
		return err
	}
	if request.Namespace != "" && request.Namespace != namespace {
		return apierrors.NewBadRequest("namespace must match the configured namespace")
	}
	if request.Kind != "" && request.Kind != "Gateway" ||
		request.APIVersion != "" && request.APIVersion != gatewayv1.GroupVersion.String() {
		return apierrors.NewBadRequest("expected gateway.networking.k8s.io/v1 Gateway")
	}
	return nil
}

func validateName(name string) error {
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return apierrors.NewBadRequest("invalid Gateway name: " + strings.Join(problems, ", "))
	}
	return nil
}
