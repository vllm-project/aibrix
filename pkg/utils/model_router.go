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

package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	modelRouterSuffix         = "-router"
	modelRouterHashLength     = 12
	maxDNS1123SubdomainLength = 253
)

// ModelRouterName returns the deterministic HTTPRoute name used for a served
// model. Existing DNS-safe model names keep the historical <model>-router form.
func ModelRouterName(modelName string) string {
	candidate := modelName + modelRouterSuffix
	if len(validation.IsDNS1123Subdomain(candidate)) == 0 {
		return candidate
	}

	var normalized strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(modelName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			normalized.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			normalized.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(normalized.String(), "-")
	if base == "" {
		base = "model"
	}

	digest := sha256.Sum256([]byte(modelName))
	hashValue := hex.EncodeToString(digest[:])[:modelRouterHashLength]
	maxBaseLength := maxDNS1123SubdomainLength - len(modelRouterSuffix) - len("-") - len(hashValue)
	if len(base) > maxBaseLength {
		base = strings.Trim(base[:maxBaseLength], "-")
	}
	return fmt.Sprintf("%s-%s%s", base, hashValue, modelRouterSuffix)
}
