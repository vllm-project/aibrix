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

package impl

import (
	"testing"

	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

func TestDefaultPlannerBackendFormatsProviderRegion(t *testing.T) {
	tests := []struct {
		name     string
		provider rmtypes.ResourceProvisionType
		region   *rmtypes.RegionSpec
		want     string
	}{
		{
			name:     "AWS",
			provider: rmtypes.ResourceProvisionTypeAWS,
			region:   rmtypes.NewAWSRegion("us-west-2", "us-west-2a"),
			want:     "us-west-2/us-west-2a",
		},
		{
			name:     "Lambda Cloud",
			provider: rmtypes.ResourceProvisionTypeLambdaCloud,
			region:   rmtypes.NewLambdaCloudRegion("us-west-2"),
			want:     "us-west-2",
		},
		{
			name:     "Kubernetes",
			provider: rmtypes.ResourceProvisionTypeKubernetes,
			region: &rmtypes.RegionSpec{Kubernetes: &rmtypes.KubernetesRegion{
				Context:   "production",
				Cluster:   "cluster.example.com",
				Namespace: "inference",
			}},
			want: "production/cluster.example.com/inference",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &defaultPlannerBackend{provider: test.provider}
			if got := backend.FormatRegion(test.region); got != test.want {
				t.Fatalf("region display = %q, want %q", got, test.want)
			}
		})
	}
}
