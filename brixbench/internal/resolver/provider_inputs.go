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
	if strings.TrimSpace(test.LocalPath) != "" {
		return fmt.Errorf("localPath is only supported for provider aibrix in %s", test.Name)
	}
	return nil
}
