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

package types

// ResourceCredential contains credentials and metadata required to provision
// resources on a specific provider.
//
// Security Note: Credential fields contain sensitive data. In production, these should be:
//   - Passed via secure environment variables
//   - Loaded from a secure secret manager at runtime
//   - Never logged or stored in plain text
//
// Example - AWS EC2:
//
//	credential:
//	  provider: aws
//	  aws:
//	    accessKeyId: "AKIAIOSFODNN7EXAMPLE"
//	    secretAccessKey: "wJalrXUtnFEMI/K7MDENG/..."
//	    region: "us-east-1"
//
// Example - Kubernetes:
//
//	credential:
//	  provider: kubernetes
//	  kubernetes:
//	    context: "my-cluster"
//	    namespace: "default"
type ResourceCredential struct {
	// Provider identifies which provisioner backend to use.
	Provider ResourceProvisionType `json:"provider"`

	// ===== Provisioner-specific credentials =====
	// Only one of these should be set based on the provider type.

	// AWS contains AWS credentials.
	// Required when provider is "aws".
	AWS *AWSCredential `json:"aws,omitempty"`

	// LambdaCloud contains Lambda Cloud credentials.
	// Required when provider is "lambdaCloud".
	LambdaCloud *LambdaCloudCredential `json:"lambdaCloud,omitempty"`

	// Kubernetes contains Kubernetes credentials.
	// Required when provider is "kubernetes".
	Kubernetes *KubernetesCredential `json:"kubernetes,omitempty"`
}

// ============================================================================
// Provisioner-Specific Credentials
// ============================================================================

// AWSCredential contains AWS EC2 credentials.
//
// Example:
//
//	credential:
//	  provider: aws
//	  aws:
//	    accessKeyId: "AKIAIOSFODNN7EXAMPLE"
//	    secretAccessKey: "wJalrXUtnFEMI/K7MDENG/..."
//	    region: "us-east-1"
type AWSCredential struct {
	// AccessKeyId is the AWS access key ID.
	// Security: Pass via secure environment variable in production.
	AccessKeyId *string `json:"accessKeyId,omitempty"`

	// SecretAccessKey is the AWS secret access key.
	// Security: Pass via secure environment variable in production.
	SecretAccessKey *string `json:"secretAccessKey,omitempty"`

	// SessionToken is the AWS session token for temporary credentials (STS).
	SessionToken *string `json:"sessionToken,omitempty"`

	// Region is the AWS region for EC2 provisioning.
	// Example: "us-east-1", "ap-southeast-1", "eu-west-1"
	Region *string `json:"region,omitempty"`
}

// LambdaCloudCredential contains Lambda Cloud credentials.
//
// Example:
//
//	credential:
//	  provider: lambdaCloud
//	  lambdaCloud:
//	    apiKey: "your-lambda-cloud-api-key"
//	    region: "us-west-2"
type LambdaCloudCredential struct {
	// ApiKey is the Lambda Cloud API key for authentication.
	ApiKey *string `json:"apiKey,omitempty"`

	// Region is the Lambda Cloud region, if regions are supported.
	Region *string `json:"region,omitempty"`
}

// KubernetesCredential contains Kubernetes credentials.
//
// Example:
//
//	credential:
//	  provider: kubernetes
//	  kubernetes:
//	    context: "my-cluster"
//	    namespace: "default"
type KubernetesCredential struct {
	// Kubeconfig is the path to kubeconfig file.
	Kubeconfig *string `json:"kubeconfig,omitempty"`

	// Context is the Kubernetes context to use.
	Context *string `json:"context,omitempty"`

	// Namespace is the default namespace.
	Namespace *string `json:"namespace,omitempty"`

	// ServiceAccountName is the service account for in-cluster auth.
	ServiceAccountName *string `json:"serviceAccountName,omitempty"`
}
