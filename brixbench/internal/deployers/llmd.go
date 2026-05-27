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
