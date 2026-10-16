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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/features"
)

type managerWithReader struct {
	manager.Manager
	reader client.Reader
}

func (m managerWithReader) GetAPIReader() client.Reader {
	return m.reader
}

type errorReader struct {
	client.Reader
	err error
}

func (r errorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

// isolateControllerRegistration isolates the global controller registration state for testing.
// It is not thread-safe; tests using it must not call t.Parallel.
func isolateControllerRegistration(t *testing.T) {
	t.Helper()

	previousAddFuncs := controllerAddFuncs
	previousEnabledControllers := features.EnabledControllers
	controllerAddFuncs = nil
	features.EnabledControllers = make(map[string]bool)

	t.Cleanup(func() {
		controllerAddFuncs = previousAddFuncs
		features.EnabledControllers = previousEnabledControllers
	})
}

func newCRDReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestInitializeSkipsDistributedControllersWithoutKubeRay(t *testing.T) {
	isolateControllerRegistration(t)
	features.InitControllers(features.DistributedInferenceController)

	err := Initialize(managerWithReader{reader: newCRDReader(t)})

	require.NoError(t, err)
	assert.Empty(t, controllerAddFuncs)
}

func TestInitializeRegistersDistributedControllersWithKubeRay(t *testing.T) {
	isolateControllerRegistration(t)
	features.InitControllers(features.DistributedInferenceController)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	crd.Name = "rayclusters.ray.io"

	err := Initialize(managerWithReader{reader: newCRDReader(t, crd)})

	require.NoError(t, err)
	assert.Len(t, controllerAddFuncs, 2)
}

func TestInitializeFailsWhenKubeRayDiscoveryFails(t *testing.T) {
	isolateControllerRegistration(t)
	features.InitControllers(features.DistributedInferenceController)
	discoveryErr := errors.New("API reader unavailable")

	err := Initialize(managerWithReader{reader: errorReader{err: discoveryErr}})

	require.ErrorIs(t, err, discoveryErr)
	assert.Empty(t, controllerAddFuncs)
}

func TestSetupWithManagerSkipsNoKindMatchError(t *testing.T) {
	isolateControllerRegistration(t)
	secondCalled := false
	controllerAddFuncs = []func(manager.Manager, config.RuntimeConfig) error{
		func(manager.Manager, config.RuntimeConfig) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "example.io", Kind: "Optional"}}
		},
		func(manager.Manager, config.RuntimeConfig) error {
			secondCalled = true
			return nil
		},
	}

	err := SetupWithManager(nil, config.NewRuntimeConfig(false, false, ""))

	require.NoError(t, err)
	assert.True(t, secondCalled)
}

func TestSetupWithManagerFailsFastOnOtherErrors(t *testing.T) {
	isolateControllerRegistration(t)
	setupErr := errors.New("setup failed")
	secondCalled := false
	controllerAddFuncs = []func(manager.Manager, config.RuntimeConfig) error{
		func(manager.Manager, config.RuntimeConfig) error {
			return setupErr
		},
		func(manager.Manager, config.RuntimeConfig) error {
			secondCalled = true
			return nil
		},
	}

	err := SetupWithManager(nil, config.NewRuntimeConfig(false, false, ""))

	require.ErrorIs(t, err, setupErr)
	assert.False(t, secondCalled)
}
