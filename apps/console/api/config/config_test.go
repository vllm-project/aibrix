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

package config

import "testing"

func TestLoadSeparatesKubernetesClusterAndWorkloadConfig(t *testing.T) {
	t.Setenv("AUTH_MODE", AuthModeDev)
	t.Setenv("KUBERNETES_KUBECONFIG", "/tmp/test-kubeconfig")
	t.Setenv("KUBERNETES_CONTEXT", "test-context")
	t.Setenv("KUBERNETES_NAMESPACE", "test-namespace")
	t.Setenv("KUBERNETES_SERVICE_TYPE", "NodePort")
	t.Setenv("KUBERNETES_CONTAINER_PORT", "9000")
	t.Setenv("KUBERNETES_SERVICE_PORT", "9001")
	t.Setenv("KUBERNETES_CPU_REQUEST", "500m")
	t.Setenv("KUBERNETES_HPA_TARGET_CPU_UTILIZATION", "70")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := cfg.KubernetesProvider; got.Kubeconfig != "/tmp/test-kubeconfig" ||
		got.Context != "test-context" || got.Namespace != "test-namespace" {
		t.Fatalf("unexpected Kubernetes provider config: %+v", got)
	}
	if got := cfg.KubernetesWorkload; got.ServiceType != "NodePort" ||
		got.ContainerPort != 9000 || got.ServicePort != 9001 ||
		got.CPURequest != "500m" || got.HPATargetCPUUtilization != 70 {
		t.Fatalf("unexpected Kubernetes workload config: %+v", got)
	}
}
