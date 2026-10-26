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

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func roleDocument(rules string) string {
	return "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\n" +
		"metadata:\n  name: controller-manager\nrules:\n" + rules
}

func TestCompareGroupedRules(t *testing.T) {
	config := roleDocument(`- apiGroups: [scheduling.godel.kubewharf.io, scheduling.volcano.sh]
  resources: [podgroups]
  verbs: ['*']
- apiGroups: ['']
  resources: [pods, services]
  verbs: [list, get]
`)
	rendered := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: release-controller-manager
---
` + strings.Replace(roleDocument(`- apiGroups: ['']
  resources: [services]
  verbs: [get, list, get]
- apiGroups: [scheduling.volcano.sh]
  resources: [podgroups]
  verbs: ['*']
- apiGroups: ['']
  resources: [pods]
  verbs: [get, list]
- apiGroups: [scheduling.godel.kubewharf.io]
  resources: [podgroups]
  verbs: ['*']
`), "name: controller-manager", "name: release-controller-manager", 1) + `---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: leader-election
rules: []
`
	require.NoError(t, compare(strings.NewReader(config), strings.NewReader(rendered)))
}

func TestComparePermissionDrift(t *testing.T) {
	rule := `- apiGroups: ['']
  resources: [pods]
  verbs: [get, list]
`
	for _, tc := range []struct {
		name, changed string
	}{
		{"missing verb", strings.Replace(rule, "[get, list]", "[get]", 1)},
		{"extra verb", strings.Replace(rule, "[get, list]", "[get, list, delete]", 1)},
		{"changed group", strings.Replace(rule, "['']", "[apps]", 1)},
		{"changed resource", strings.Replace(rule, "[pods]", "[secrets]", 1)},
		{"wildcard verb", strings.Replace(rule, "[get, list]", "['*']", 1)},
		{"restricted names", rule + "  resourceNames: [one-pod]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compare(strings.NewReader(roleDocument(rule)), strings.NewReader(roleDocument(tc.changed)))
			require.ErrorContains(t, err, "RBAC rules differ")
		})
	}
}

func TestResourceNamesAndNonResourceURLs(t *testing.T) {
	grouped := roleDocument(`- apiGroups: ['']
  resources: [pods]
  resourceNames: [one, two]
  verbs: [get]
- nonResourceURLs: [/healthz, /metrics]
  verbs: [get]
`)
	split := roleDocument(`- apiGroups: ['']
  resources: [pods]
  resourceNames: [two]
  verbs: [get]
- nonResourceURLs: [/metrics]
  verbs: [get]
- apiGroups: ['']
  resources: [pods]
  resourceNames: [one]
  verbs: [get]
- nonResourceURLs: [/healthz]
  verbs: [get]
`)
	require.NoError(t, compare(strings.NewReader(grouped), strings.NewReader(split)))
	for _, changed := range []string{
		strings.Replace(split, "resourceNames: [two]", "resourceNames: ['*']", 1),
		strings.Replace(split, "[/metrics]", "[/metrics/*]", 1),
		strings.Replace(split, "resourceNames: [two]", "resourceNames: []", 1),
	} {
		require.Error(t, compare(strings.NewReader(grouped), strings.NewReader(changed)))
	}
}

func TestRejectMissingAmbiguousOrInvalidRoles(t *testing.T) {
	valid := roleDocument("- apiGroups: ['']\n  resources: [pods]\n  verbs: [get]\n")
	for _, tc := range []struct {
		name, document string
	}{
		{"missing", "apiVersion: v1\nkind: ServiceAccount\n"},
		{"multiple", valid + "---\n" + valid},
		{"syntax", "kind: ["},
		{"wrong API version", strings.Replace(valid, "rbac.authorization.k8s.io/v1", "v1", 1)},
		{"no name", strings.Replace(valid, "name: controller-manager", "name: ''", 1)},
		{"aggregated", valid + "aggregationRule: {clusterRoleSelectors: []}\n"},
		{"unknown rule field", valid + "  unexpected: true\n"},
		{"missing groups", strings.Replace(valid, "apiGroups: ['']", "apiGroups: []", 1)},
		{"missing resources", strings.Replace(valid, "resources: [pods]", "resources: []", 1)},
		{"missing verbs", strings.Replace(valid, "verbs: [get]", "verbs: []", 1)},
		{"mixed rule", valid + "  nonResourceURLs: [/healthz]\n"},
		{"empty URL", roleDocument("- nonResourceURLs: ['']\n  verbs: [get]\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compare(strings.NewReader(valid), strings.NewReader(tc.document))
			require.Error(t, err)
		})
	}
}
