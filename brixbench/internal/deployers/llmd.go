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
	"fmt"
)

// LLMdDeployer is a placeholder deployer for future LLM-d support.
type LLMdDeployer struct {
	namespace string
}

var _ Deployer = (*LLMdDeployer)(nil)

// NewLLMdDeployer creates a placeholder deployer for LLM-d.
func NewLLMdDeployer() *LLMdDeployer {
	return &LLMdDeployer{}
}

func (d *LLMdDeployer) Initialize(ctx context.Context, config Config) error {
	d.namespace = config.Namespace
	return nil
}

func (d *LLMdDeployer) DeployControlPlane(ctx context.Context) error {
	return fmt.Errorf("LLM-d deployer is not implemented")
}

func (d *LLMdDeployer) DeployGateway(ctx context.Context) error {
	return fmt.Errorf("LLM-d deployer is not implemented")
}

func (d *LLMdDeployer) DeployEngine(ctx context.Context) error {
	return fmt.Errorf("LLM-d deployer is not implemented")
}

func (d *LLMdDeployer) WaitForReady(ctx context.Context) error {
	return fmt.Errorf("LLM-d deployer is not implemented")
}

func (d *LLMdDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	return "", fmt.Errorf("LLM-d deployer is not implemented")
}

func (d *LLMdDeployer) CaptureArtifacts(ctx context.Context) error {
	return nil
}

func (d *LLMdDeployer) Teardown(ctx context.Context) error {
	return fmt.Errorf("LLM-d deployer is not implemented")
}
