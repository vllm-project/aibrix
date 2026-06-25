package resolver

import (
	"fmt"
	"regexp"
	"strings"
)

var stableDynamoVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

func validateDynamoSourceSelection(test *Test) error {
	if strings.TrimSpace(test.Commit) != "" {
		return fmt.Errorf("provider dynamo only supports version, not commit, for %s", test.Name)
	}
	if strings.TrimSpace(test.LocalPath) != "" {
		return fmt.Errorf("provider dynamo only supports version, not localPath, for %s", test.Name)
	}
	if len(test.ControlPlane) > 0 {
		return fmt.Errorf("provider dynamo installs control plane from release version; controlplane is not supported for %s", test.Name)
	}

	normalizedVersion, err := normalizeDynamoVersion(test.Version)
	if err != nil {
		return fmt.Errorf("%w for %s", err, test.Name)
	}
	test.Version = normalizedVersion
	return nil
}

func normalizeDynamoVersion(version string) (string, error) {
	normalized := strings.TrimSpace(version)
	if normalized == "" {
		return "", fmt.Errorf("missing Dynamo version")
	}
	if !stableDynamoVersionPattern.MatchString(normalized) {
		return "", fmt.Errorf("invalid Dynamo version %q: expected stable semver like v1.2.1", version)
	}
	if !strings.HasPrefix(normalized, "v") {
		normalized = "v" + normalized
	}
	return normalized, nil
}
