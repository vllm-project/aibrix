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

package constants

const (
	// AppLabelManagedBy identifies the controller or product managing a resource.
	AppLabelManagedBy = "app.kubernetes.io/managed-by"

	// ConsoleManagedByValue marks resources created and managed by AIBrix Console.
	ConsoleManagedByValue = "aibrix-console"

	// ConsoleDeploymentIDAnnotation links a Kubernetes resource to the canonical
	// Console deployment object used by the API and frontend route.
	ConsoleDeploymentIDAnnotation = "console.aibrix.ai/deployment-id"

	// ConsoleDeploymentNameAnnotation preserves the user-facing Console name.
	ConsoleDeploymentNameAnnotation = "console.aibrix.ai/deployment-name"
)
