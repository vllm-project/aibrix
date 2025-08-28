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

package validation

import orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"

// TODO: Put some common validation logic here: e.g ValidateRoleSet ValidateRoleSetStatusEqualTo ...

func FindRoleStatus(rs *orchestrationapi.RoleSet, roleName string) *orchestrationapi.RoleStatus {
	for i := range rs.Status.Roles {
		if rs.Status.Roles[i].Name == roleName {
			return &rs.Status.Roles[i]
		}
	}
	return nil
}

func FindCondition(condType string, c []orchestrationapi.Condition) *orchestrationapi.Condition {
	for i := range c {
		if string(c[i].Type) == condType {
			cond := c[i] // copy
			return &cond
		}
	}
	return nil
}
