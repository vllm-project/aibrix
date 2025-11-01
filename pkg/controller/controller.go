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

package controller

import (
	"context"
	"fmt"

	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache"
	"github.com/vllm-project/aibrix/pkg/controller/modeladapter"
	"github.com/vllm-project/aibrix/pkg/controller/modelrouter"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler"
	"github.com/vllm-project/aibrix/pkg/controller/podset"
	"github.com/vllm-project/aibrix/pkg/controller/rayclusterfleet"
	"github.com/vllm-project/aibrix/pkg/controller/rayclusterreplicaset"
	"github.com/vllm-project/aibrix/pkg/controller/roleset"
	"github.com/vllm-project/aibrix/pkg/controller/stormservice"
	"github.com/vllm-project/aibrix/pkg/features"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Borrowed logic from Kruise
// Original source: https://github.com/openkruise/kruise/blob/master/pkg/controller/controllers.go
// Reason: We have single controller-manager as well and use the controller-runtime libraries.
// 		   Instead of registering every controller in the main.go, kruise's registration flow is much cleaner.

var controllerAddFuncs []func(manager.Manager, config.RuntimeConfig) error

func Initialize(mgr manager.Manager) error {
	if features.IsControllerEnabled(features.PodAutoscalerController) {
		controllerAddFuncs = append(controllerAddFuncs, podautoscaler.Add)
	}

	if features.IsControllerEnabled(features.ModelAdapterController) {
		controllerAddFuncs = append(controllerAddFuncs, modeladapter.Add)
	}

	if features.IsControllerEnabled(features.ModelRouteController) {
		controllerAddFuncs = append(controllerAddFuncs, modelrouter.Add)
	}

	if features.IsControllerEnabled(features.DistributedInferenceController) {
		// Check if the KubeRay CRD exists. Only skip if CRD is not found.
		// For other errors (RBAC, API server issues), fail fast.
		crdName := "rayclusters.ray.io"
		exists, err := checkCRDExists(mgr.GetAPIReader(), crdName)
		if err != nil {
			// For errors other than NotFound (e.g., RBAC permissions, API server issues), fail fast
			return fmt.Errorf("failed to check for KubeRay CRD %s: %w", crdName, err)
		}
		if !exists {
			klog.InfoS("KubeRay CRD not found, skipping distributed inference controller. "+
				"This is optional - install KubeRay operator if you need RayClusterFleet/RayClusterReplicaSet support.",
				"CRD", crdName)
			// Don't add the controller functions, effectively disabling this controller
		} else {
			// CRD found, enable the controllers
			controllerAddFuncs = append(controllerAddFuncs, rayclusterreplicaset.Add)
			controllerAddFuncs = append(controllerAddFuncs, rayclusterfleet.Add)
		}
	}

	if features.IsControllerEnabled(features.KVCacheController) {
		controllerAddFuncs = append(controllerAddFuncs, kvcache.Add)
	}

	if features.IsControllerEnabled(features.StormServiceController) {
		controllerAddFuncs = append(controllerAddFuncs, roleset.Add)
		controllerAddFuncs = append(controllerAddFuncs, stormservice.Add)
		controllerAddFuncs = append(controllerAddFuncs, podset.Add)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func SetupWithManager(m manager.Manager, runtimeConfig config.RuntimeConfig) error {
	for _, f := range controllerAddFuncs {
		if err := f(m, runtimeConfig); err != nil {
			if kindMatchErr, ok := err.(*meta.NoKindMatchError); ok {
				klog.InfoS("CRD is not installed, its controller will perform noops!", "CRD", kindMatchErr.GroupKind)
				continue
			}
			return err
		}
	}
	return nil
}

// checkCRDExists checks if the specified CRD exists in the cluster.
// Returns (exists bool, error). If error is not nil, exists value should be ignored.
func checkCRDExists(c client.Reader, crdName string) (bool, error) {
	crd := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
	}

	err := c.Get(context.TODO(), client.ObjectKey{Name: crdName}, crd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("error checking CRD %q: %w", crdName, err)
	}
	return true, nil
}
