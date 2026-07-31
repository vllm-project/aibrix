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

package patch

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func RemoveFinalizerPatch(obj metav1.Object, finalizers ...string) client.Patch {
	var (
		jsonPatches = NewJSONPatch()
		needDeleted = sets.NewString(finalizers...)
		filtered    = []string{}
	)

	for _, k := range obj.GetFinalizers() {
		if !needDeleted.Has(k) {
			filtered = append(filtered, k)
		}
	}

	if len(filtered) == 0 {
		filtered = nil
	}

	if len(filtered) == 0 {
		jsonPatches.Append(Remove, "/metadata/finalizers", nil)
	} else {
		jsonPatches.Append(Add, "/metadata/finalizers", filtered)
	}
	patchBytes, _ := jsonPatches.Marshal()
	return client.RawPatch(types.JSONPatchType, patchBytes)
}

func AddFinalizerPatch(obj metav1.Object, finalizers ...string) client.Patch {
	var (
		jsonPatches = NewJSONPatch()
	)

	res := sets.NewString(append(obj.GetFinalizers(), finalizers...)...).List()
	jsonPatches.Append(Add, "/metadata/finalizers", res)
	patchBytes, _ := jsonPatches.Marshal()
	return client.RawPatch(types.JSONPatchType, patchBytes)
}
