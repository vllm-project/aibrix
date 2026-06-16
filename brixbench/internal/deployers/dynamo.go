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
