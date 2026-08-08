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

package deployers

import (
	"context"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

type Config struct {
	ControlPlanePaths      []string
	EnginePath             string
	Namespace              string
	LogDir                 string
	ProjectRoot            string
	FullStack              bool
	VKEDev                 bool
	ResolvedCommit         string
	WorkspacePath          string
	GatewayImageRepository string
	GatewayImageTag        string
	GatewayEnv             map[string]string
	GatewayResourceFiles   []string
	PlatformValuesFile     string
	TestCase               *resolver.Test
}

// Deployer manages the lifecycle of the test environment (Control Plane + Engine).
// New control planes (e.g., llmd, dynamo) can be added by implementing this interface.
type Deployer interface {
	// Initialize prepares the deployer with the given configuration paths without actually deploying them.
	Initialize(ctx context.Context, config Config) error

	// DeployControlPlane deploys the control plane (e.g., configuring aibrix-system namespace).
	DeployControlPlane(ctx context.Context) error

	// DeployGateway deploys the traffic routing layer.
	DeployGateway(ctx context.Context) error

	// DeployEngine deploys the inference engine (e.g., creating Pods/Deployments).
	DeployEngine(ctx context.Context) error

	// WaitForReady checks if the engine is ready to receive traffic.
	WaitForReady(ctx context.Context) error

	// GetGatewayEndpoint returns the dynamically resolved URL for the Gateway.
	// E.g., http://10.96.x.x:80 or http://localhost:8080 (if port-forwarded)
	GetGatewayEndpoint(ctx context.Context) (string, error)

	// CaptureArtifacts writes optional run artifacts after the test outcome is known.
	CaptureArtifacts(ctx context.Context) error

	// Teardown is optional and kept for future use if manual cleanup is needed outside the test loop.
	Teardown(ctx context.Context) error
}
