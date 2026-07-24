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
