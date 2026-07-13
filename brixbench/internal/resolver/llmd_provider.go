package resolver

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var stableLLMdVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

func validateLLMdSourceSelection(test *Test) error {
	if strings.TrimSpace(test.Commit) != "" {
		return fmt.Errorf("provider llmd only supports version, not commit, for %s", test.Name)
	}
	if strings.TrimSpace(test.LocalPath) != "" {
		return fmt.Errorf("provider llmd only supports LLMD_REPO env override, not localPath, for %s", test.Name)
	}
	if strings.TrimSpace(test.Platform.ValuesFile) != "" {
		return fmt.Errorf("provider llmd uses controlplane values files; platform.valuesFile is not supported for %s", test.Name)
	}
	if len(test.ControlPlane) == 0 {
		return fmt.Errorf("provider llmd requires at least one controlplane values file for %s", test.Name)
	}
	if err := validateLLMdControlPlaneFiles(test); err != nil {
		return err
	}

	normalizedVersion, err := normalizeLLMdVersion(test.Version)
	if err != nil {
		return fmt.Errorf("%w for %s", err, test.Name)
	}
	test.Version = normalizedVersion
	return nil
}

func normalizeLLMdVersion(version string) (string, error) {
	normalized := strings.TrimSpace(version)
	if normalized == "" {
		return "", fmt.Errorf("missing LLM-d version")
	}
	if !stableLLMdVersionPattern.MatchString(normalized) {
		return "", fmt.Errorf("invalid LLM-d version %q: expected stable semver like v0.8.1", version)
	}
	if !strings.HasPrefix(normalized, "v") {
		normalized = "v" + normalized
	}
	return normalized, nil
}

func validateLLMdControlPlaneFiles(test *Test) error {
	for i, valuesFile := range test.ControlPlane {
		valuesFile = strings.TrimSpace(valuesFile)
		if valuesFile == "" {
			return fmt.Errorf("provider llmd controlplane values file %d is empty for %s", i, test.Name)
		}
		test.ControlPlane[i] = valuesFile

		info, err := os.Stat(valuesFile)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("provider llmd controlplane values file not found for %s: %s", test.Name, valuesFile)
			}
			return fmt.Errorf("failed to inspect provider llmd controlplane values file for %s: %w", test.Name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("provider llmd controlplane values file must be a file for %s: %s", test.Name, valuesFile)
		}
	}
	return nil
}
