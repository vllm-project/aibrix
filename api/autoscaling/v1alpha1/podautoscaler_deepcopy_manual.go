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

package v1alpha1

// NOTE: This file contains manually written DeepCopy methods for RolePolicy.
// controller-gen does not generate DeepCopy methods for nested struct types like RolePolicy,
// so these methods must be manually provided to support the generated DeepCopy code in
// PodAutoscalerSpec.DeepCopyInto which calls (*in)[i].DeepCopyInto(&(*out)[i]).
// This is a normal and expected pattern in Kubernetes codegen.

// DeepCopyInto is a manually written deepcopy function for RolePolicy.
func (in *RolePolicy) DeepCopyInto(out *RolePolicy) {
	*out = *in
	if in.MinReplicas != nil {
		in, out := &in.MinReplicas, &out.MinReplicas
		*out = new(int32)
		**out = **in
	}
	if in.MetricsSources != nil {
		in, out := &in.MetricsSources, &out.MetricsSources
		*out = make([]MetricSource, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is a manually written deepcopy function for RolePolicy.
func (in *RolePolicy) DeepCopy() *RolePolicy {
	if in == nil {
		return nil
	}
	out := new(RolePolicy)
	in.DeepCopyInto(out)
	return out
}
