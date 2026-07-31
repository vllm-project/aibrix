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

package resolver

import (
	"fmt"
	"strings"
)

func validateProviderInputs(test *Test) error {
	switch test.ProviderName() {
	case "":
		return validateNullProviderSourceSelection(test)
	case "aibrix":
		return validateAIBrixSourceSelection(test)
	case "dynamo":
		return validateDynamoSourceSelection(test)
	case "llmd":
		return fmt.Errorf("provider llmd is not implemented for %s", test.Name)
	default:
		return fmt.Errorf("unknown provider %q for %s", test.ProviderName(), test.Name)
	}
}

func validateNullProviderSourceSelection(test *Test) error {
	if strings.TrimSpace(test.Version) != "" || strings.TrimSpace(test.Commit) != "" || len(test.ControlPlane) > 0 || strings.TrimSpace(test.Platform.ValuesFile) != "" {
		return fmt.Errorf("provider null does not support version, commit, controlplane, or platform inputs for %s", test.Name)
	}
	if strings.TrimSpace(test.LocalPath) != "" {
		return fmt.Errorf("localPath is only supported for provider aibrix in %s", test.Name)
	}
	return nil
}
