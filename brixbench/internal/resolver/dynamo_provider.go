package resolver

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := validateDynamoPlatformValuesFile(test); err != nil {
		return err
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

func validateDynamoPlatformValuesFile(test *Test) error {
	valuesFile := strings.TrimSpace(test.Platform.ValuesFile)
	if valuesFile == "" {
		return nil
	}
	test.Platform.ValuesFile = valuesFile

	cleanPath := filepath.ToSlash(filepath.Clean(valuesFile))
	if cleanPath == ".tmp/dynamo-reference" ||
		strings.HasPrefix(cleanPath, ".tmp/dynamo-reference/") ||
		strings.Contains(cleanPath, "/.tmp/dynamo-reference/") {
		return fmt.Errorf("provider dynamo platform.valuesFile must not reference .tmp/dynamo-reference for %s", test.Name)
	}

	info, err := os.Stat(valuesFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("provider dynamo platform.valuesFile not found for %s: %s", test.Name, valuesFile)
		}
		return fmt.Errorf("failed to inspect provider dynamo platform.valuesFile for %s: %w", test.Name, err)
	}
	if info.IsDir() {
		return fmt.Errorf("provider dynamo platform.valuesFile must be a file for %s: %s", test.Name, valuesFile)
	}
	return nil
}
