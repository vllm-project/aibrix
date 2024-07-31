package controller

import (
	"github.com/aibrix/aibrix/pkg/controller/modeladapter"
	"github.com/aibrix/aibrix/pkg/controller/modelrouter"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Borrowed logic from Kruise
// Original source: https://github.com/openkruise/kruise/blob/master/pkg/controller/controllers.go
// Reason: We have single controller-manager as well and use the controller-runtime libraries.
// 		   Instead of registering every controller in the main.go, kruise's registration flow is much cleaner.

var controllerAddFuncs []func(manager.Manager) error

func init() {
	controllerAddFuncs = append(controllerAddFuncs, podautoscaler.Add)
	controllerAddFuncs = append(controllerAddFuncs, modeladapter.Add)
	controllerAddFuncs = append(controllerAddFuncs, modelrouter.Add)
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
