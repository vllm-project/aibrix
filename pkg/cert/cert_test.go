/*
Copyright 2024 The Aibrix Team.

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

package cert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rotator "github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

const configDir = "../../config"

func TestWebhookNamesForDefaultPrefix(t *testing.T) {
	// The default install, the Helm chart and the installation smoke test all
	// use these names, so the default must not change them.
	assert.Equal(t, WebhookNames{
		Service:                        "aibrix-webhook-service",
		Secret:                         "aibrix-webhook-server-cert",
		ValidatingWebhookConfiguration: "aibrix-validating-webhook-configuration",
		MutatingWebhookConfiguration:   "aibrix-mutating-webhook-configuration",
	}, WebhookNamesForPrefix(DefaultWebhookNamePrefix))
}

func TestWebhookNamesForPrefix(t *testing.T) {
	assert.Equal(t, WebhookNames{
		Service:                        "aibrix-lora-webhook-service",
		Secret:                         "aibrix-lora-webhook-server-cert",
		ValidatingWebhookConfiguration: "aibrix-lora-validating-webhook-configuration",
		MutatingWebhookConfiguration:   "aibrix-lora-mutating-webhook-configuration",
	}, WebhookNamesForPrefix("aibrix-lora-"))
}

func TestNewCertRotatorUsesThePrefixedNames(t *testing.T) {
	ready := make(chan struct{})
	r := newCertRotator("aibrix-system", "aibrix-lora-", ready)

	assert.Equal(t, types.NamespacedName{Namespace: "aibrix-system", Name: "aibrix-lora-webhook-server-cert"}, r.SecretKey)
	assert.Equal(t, "aibrix-lora-webhook-service.aibrix-system.svc", r.DNSName)
	assert.Equal(t, []rotator.WebhookInfo{
		{Type: rotator.Validating, Name: "aibrix-lora-validating-webhook-configuration"},
		{Type: rotator.Mutating, Name: "aibrix-lora-mutating-webhook-configuration"},
	}, r.Webhooks)
	assert.Equal(t, certDir, r.CertDir)
	assert.Equal(t, ready, r.IsReady)
}

// TestWebhookBaseNamesMatchManifests checks the base names against the objects
// the installs render, so that prefix + base name is the name kustomize gives
// each object.
func TestWebhookBaseNamesMatchManifests(t *testing.T) {
	assert.Equal(t, []string{serviceBaseName}, manifestNames(t, "webhook/service.yaml", "Service"))
	assert.Equal(t, []string{secretBaseName}, manifestNames(t, "internalcert/secret.yaml", "Secret"))
	assert.Equal(t, []string{validateWebhookConfBaseName},
		manifestNames(t, "webhook/manifests.yaml", "ValidatingWebhookConfiguration"))
	assert.Equal(t, []string{mutatingWebhookConfBaseName},
		manifestNames(t, "webhook/manifests.yaml", "MutatingWebhookConfiguration"))
}

// TestInstallsPassTheirWebhookNamePrefix guards against an install whose
// webhook objects the manager can't find (#3565). An install that ships the
// webhook objects must keep the webhook enabled and pass its kustomize
// namePrefix as --webhook-name-prefix (or leave the default, if that is its
// prefix). An install that doesn't ship them must pass --disable-webhook,
// because otherwise the cert rotator looks for a Secret that doesn't exist.
func TestInstallsPassTheirWebhookNamePrefix(t *testing.T) {
	installs := []string{"default"}
	overlays, err := filepath.Glob(filepath.Join(configDir, "standalone", "*", "kustomization.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, overlays)
	for _, k := range overlays {
		rel, err := filepath.Rel(configDir, filepath.Dir(k))
		require.NoError(t, err)
		installs = append(installs, rel)
	}

	for _, install := range installs {
		t.Run(install, func(t *testing.T) {
			k := readKustomization(t, install)
			args := managerArgs(t, install, k)

			disabled := false
			prefix := DefaultWebhookNamePrefix
			for _, arg := range args {
				switch {
				case arg == "--disable-webhook" || arg == "--disable-webhook=true":
					disabled = true
				case strings.HasPrefix(arg, "--webhook-name-prefix="):
					prefix = strings.TrimPrefix(arg, "--webhook-name-prefix=")
				}
			}

			if !k.shipsWebhooks() {
				assert.True(t, disabled, "%s doesn't ship the webhook objects, so its manager must pass --disable-webhook", install)
				return
			}
			assert.False(t, disabled, "%s ships the webhook objects, so its manager must not pass --disable-webhook", install)
			assert.Equal(t, k.NamePrefix, prefix,
				"%s names its webhook objects with namePrefix %q, so its manager must pass --webhook-name-prefix=%s",
				install, k.NamePrefix, k.NamePrefix)
		})
	}
}

// kustomization is the part of a kustomization.yaml that the guard reads:
// the top-level namePrefix, resources and patches. It doesn't follow nested
// bases or components, and ignores the deprecated patchesStrategicMerge and
// patchesJson6902 fields. Extend it if an install starts setting its prefix,
// its webhook resources or its manager args through one of those.
type kustomization struct {
	NamePrefix string   `json:"namePrefix"`
	Resources  []string `json:"resources"`
	Patches    []struct {
		Path   string `json:"path"`
		Patch  string `json:"patch"`
		Target *struct {
			Kind string `json:"kind"`
		} `json:"target"`
	} `json:"patches"`
}

// shipsWebhooks reports whether the install includes the webhook
// configurations and the webhook Service from config/webhook.
func (k kustomization) shipsWebhooks() bool {
	for _, r := range k.Resources {
		if filepath.Base(r) == "webhook" {
			return true
		}
	}
	return false
}

func readKustomization(t *testing.T, install string) kustomization {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, install, "kustomization.yaml"))
	require.NoError(t, err)
	var k kustomization
	require.NoError(t, yaml.Unmarshal(data, &k))
	return k
}

// managerArgs returns the manager container's args as the install renders
// them: a Deployment patch that sets args replaces the list in
// config/manager/manager.yaml, and one that doesn't leaves it as it is.
func managerArgs(t *testing.T, install string, k kustomization) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, "manager", "manager.yaml"))
	require.NoError(t, err)
	var args []string
	for _, doc := range strings.Split(string(data), "\n---") {
		a, ok, err := managerContainerArgs([]byte(doc))
		require.NoError(t, err)
		if ok {
			args = a
		}
	}
	require.NotEmpty(t, args, "config/manager/manager.yaml has no manager container args")

	for _, p := range k.Patches {
		patch := []byte(p.Patch)
		if p.Path != "" {
			patch, err = os.ReadFile(filepath.Join(configDir, install, p.Path))
			require.NoError(t, err)
		}
		a, ok, err := managerContainerArgs(patch)
		if err != nil {
			// Not a strategic merge patch: a JSON 6902 patch is a list of
			// operations. Skipping one is only safe if it can't change the
			// manager's args, so fail on one that targets a Deployment.
			source := p.Path
			if source == "" {
				source = "inline patch"
			}
			require.False(t, p.Target != nil && p.Target.Kind == "Deployment",
				"%s: can't read the JSON 6902 Deployment patch (%s); teach managerArgs about it", install, source)
			continue
		}
		if ok && a != nil {
			args = a
		}
	}
	return args
}

// managerContainerArgs returns the args of the container named "manager" if
// doc is a Deployment that has one. Strategic merge patches of other kinds
// decode and are skipped; err is set for a document that isn't an object,
// such as a JSON 6902 patch.
func managerContainerArgs(doc []byte) ([]string, bool, error) {
	var d appsv1.Deployment
	if err := yaml.Unmarshal(doc, &d); err != nil {
		return nil, false, err
	}
	if d.Kind != "Deployment" {
		return nil, false, nil
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c.Args, true, nil
		}
	}
	return nil, false, nil
}

// manifestNames returns the names of the objects of the given kind in a file
// under config/.
func manifestNames(t *testing.T, file, kind string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, file))
	require.NoError(t, err)
	var names []string
	for _, doc := range strings.Split(string(data), "\n---") {
		var obj struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		if obj.Kind == kind {
			names = append(names, obj.Name)
		}
	}
	return names
}
