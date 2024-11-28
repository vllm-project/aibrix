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
	"github.com/aibrix/aibrix/pkg/controller/modeladapter"
	"github.com/aibrix/aibrix/pkg/controller/modelrouter"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler"
	"github.com/aibrix/aibrix/pkg/controller/rayclusterfleet"
	"github.com/aibrix/aibrix/pkg/controller/rayclusterreplicaset"
	"github.com/aibrix/aibrix/pkg/features"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Borrowed logic from Kruise
// Original source: https://github.com/openkruise/kruise/blob/master/pkg/controller/controllers.go
// Reason: We have single controller-manager as well and use the controller-runtime libraries.
// 		   Instead of registering every controller in the main.go, kruise's registration flow is much cleaner.

var controllerAddFuncs []func(manager.Manager) error

func Initialize() {
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
		// TODO: only enable them if KubeRay is installed (check RayCluster CRD exist)
		controllerAddFuncs = append(controllerAddFuncs, rayclusterreplicaset.Add)
		controllerAddFuncs = append(controllerAddFuncs, rayclusterfleet.Add)
	}
}

// SetupWithManager sets up the controller with the Manager.
func SetupWithManager(m manager.Manager) error {
	for _, f := range controllerAddFuncs {
		if err := f(m); err != nil {
			if kindMatchErr, ok := err.(*meta.NoKindMatchError); ok {
				klog.InfoS("CRD is not installed, its controller will perform noops!", "CRD", kindMatchErr.GroupKind)
				continue
			}
			return err
		}
	}
	return nil
}
