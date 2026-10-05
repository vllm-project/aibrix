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

func validateAIBrixSourceSelection(test *Test) error {
	if len(test.ControlPlane) > 0 {
		return fmt.Errorf("provider aibrix does not support controlplane source input for %s; use version, commit, or localPath", test.Name)
	}

	selected := make([]string, 0, 3)
	if strings.TrimSpace(test.Version) != "" {
		selected = append(selected, "version")
	}
	if strings.TrimSpace(test.Commit) != "" {
		selected = append(selected, "commit")
	}
	if strings.TrimSpace(test.LocalPath) != "" {
		selected = append(selected, "localPath")
	}

	switch len(selected) {
	case 0:
		return fmt.Errorf("provider aibrix requires exactly one source input for %s: version, commit, or localPath", test.Name)
	case 1:
		return nil
	default:
		return fmt.Errorf("provider aibrix source inputs are mutually exclusive for %s: %s", test.Name, strings.Join(selected, ", "))
	}
}
