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

package transfer

import (
	"errors"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// ConnectorTypeIdentifier is the pod label that specifies the KV transfer backend.
	ConnectorTypeIdentifier = "model.aibrix.ai/kv-connector-type"

	ConnectorTypeSHFS     = "shfs"
	ConnectorTypeNIXL     = "nixl"
	ConnectorTypeMooncake = "mooncake"
)

// ResolveConnectorType resolves the effective connector type from a pod label value,
// falling back to globalDefault when the label is absent or unrecognized.
func ResolveConnectorType(podLabelValue, globalDefault string) string {
	v := strings.ToLower(podLabelValue)
	switch v {
	case ConnectorTypeSHFS, ConnectorTypeNIXL, ConnectorTypeMooncake:
		return v
	default:
		if podLabelValue != "" {
			klog.Warningf("unrecognized kv-connector-type label %q, falling back to global config (%s)", podLabelValue, globalDefault)
		}
		return globalDefault
	}
}

// ResolveAgentForPod resolves the KVTransferAgent for a pod. It checks the
// ConnectorTypeIdentifier pod label first, then falls back to globalDefault.
func ResolveAgentForPod(pod *v1.Pod, globalDefault string) (KVTransferAgent, error) {
	if pod == nil {
		return nil, errors.New("cannot resolve KV transfer agent: pod is nil")
	}
	connectorType := ResolveConnectorType(pod.Labels[ConnectorTypeIdentifier], globalDefault)
	return Resolve(connectorType)
}
