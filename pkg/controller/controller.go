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
	"github.com/vllm-project/aibrix/pkg/utils"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/klog/v2"
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
		exists, err := utils.CheckCRDExists(mgr.GetAPIReader(), crdName)
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
