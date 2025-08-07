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

package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// nolint:unused
// log is for logging in this package.
var kvcachelog = logf.Log.WithName("kvcache-resource")

// SetupKVCacheWebhookWithManager registers the webhook for KVCache in the manager.
func SetupKVCacheWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&orchestrationv1alpha1.KVCache{}).
		WithValidator(&KVCacheCustomValidator{}).
		WithDefaulter(&KVCacheCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-orchestration-aibrix-ai-v1alpha1-kvcache,mutating=true,failurePolicy=fail,sideEffects=None,groups=orchestration.aibrix.ai,resources=kvcaches,verbs=create;update,versions=v1alpha1,name=mkvcache-v1alpha1.kb.io,admissionReviewVersions=v1

// KVCacheCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind KVCache when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type KVCacheCustomDefaulter struct {
}

var _ webhook.CustomDefaulter = &KVCacheCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind KVCache.
func (d *KVCacheCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	kvcache, ok := obj.(*orchestrationv1alpha1.KVCache)
	if !ok {
		return fmt.Errorf("expected a KVCache object but got %T", obj)
	}

	if kvcache.Annotations == nil {
		kvcache.Annotations = map[string]string{}
	}

	backend := kvcache.Annotations[constants.KVCacheLabelKeyBackend]
	if backend == "" {
		kvcache.Annotations[constants.KVCacheLabelKeyBackend] = constants.KVCacheBackendDefault
	}

	mode := kvcache.Annotations[constants.KVCacheAnnotationMode]
	if mode == "" {
		kvcache.Annotations[constants.KVCacheAnnotationMode] = constants.KVCacheBackendDefault
	}
	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-orchestration-aibrix-ai-v1alpha1-kvcache,mutating=false,failurePolicy=fail,sideEffects=None,groups=orchestration.aibrix.ai,resources=kvcaches,verbs=create;update,versions=v1alpha1,name=vkvcache-v1alpha1.kb.io,admissionReviewVersions=v1

// KVCacheCustomValidator struct is responsible for validating the KVCache resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type KVCacheCustomValidator struct {
}

var _ webhook.CustomValidator = &KVCacheCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type KVCache.
func (v *KVCacheCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	kvcache, ok := obj.(*orchestrationv1alpha1.KVCache)
	if !ok {
		return nil, fmt.Errorf("expected a KVCache object but got %T", obj)
	}
	kvcachelog.Info("Validation for KVCache upon creation", "name", kvcache.GetName())
	err := utils.ValidateKVCacheBackend(kvcache)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type KVCache.
func (v *KVCacheCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type KVCache.
func (v *KVCacheCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
