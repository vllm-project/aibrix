package deployers

import (
	"context"
	"fmt"
)

// DynamoDeployer is a placeholder deployer for future Dynamo support.
type DynamoDeployer struct {
	namespace string
}

var _ Deployer = (*DynamoDeployer)(nil)

// NewDynamoDeployer creates a placeholder deployer for Dynamo.
func NewDynamoDeployer() *DynamoDeployer {
	return &DynamoDeployer{}
}

func (d *DynamoDeployer) Initialize(ctx context.Context, config Config) error {
	d.namespace = config.Namespace
	return nil
}

func (d *DynamoDeployer) DeployControlPlane(ctx context.Context) error {
	return fmt.Errorf("Dynamo deployer is not implemented")
}

func (d *DynamoDeployer) DeployGateway(ctx context.Context) error {
	return fmt.Errorf("Dynamo deployer is not implemented")
}

func (d *DynamoDeployer) DeployEngine(ctx context.Context) error {
	return fmt.Errorf("Dynamo deployer is not implemented")
}

func (d *DynamoDeployer) WaitForReady(ctx context.Context) error {
	return fmt.Errorf("Dynamo deployer is not implemented")
}

func (d *DynamoDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	return "", fmt.Errorf("Dynamo deployer is not implemented")
}

func (d *DynamoDeployer) CaptureArtifacts(ctx context.Context) error {
	return nil
}

func (d *DynamoDeployer) Teardown(ctx context.Context) error {
	return fmt.Errorf("Dynamo deployer is not implemented")
}
