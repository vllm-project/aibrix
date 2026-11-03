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

// Package contract defines Console operations on Gateway API resources.
package contract

import (
	"context"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Provider manages Gateways in one configured cluster and namespace. Names are
// namespace-local; callers cannot select another cluster or namespace.
//
// Create and Update return the persisted desired state, not a readiness
// guarantee. Status belongs to the Gateway controller. Update requires the
// resourceVersion returned by Get and must reject conflicting writes.
//
// Implementations return Kubernetes API errors for invalid input, missing
// resources, conflicts and unsupported operations.
type Provider interface {
	List(context.Context) (*gatewayv1.GatewayList, error)
	Get(context.Context, string) (*gatewayv1.Gateway, error)
	Create(context.Context, *gatewayv1.Gateway) (*gatewayv1.Gateway, error)
	Update(context.Context, *gatewayv1.Gateway) (*gatewayv1.Gateway, error)
}
