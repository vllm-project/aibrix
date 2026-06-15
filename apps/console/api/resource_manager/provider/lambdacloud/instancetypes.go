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

package lambdacloud

import (
	"fmt"
	"strings"
)

// NormalizeGPUType canonicalizes an accelerator name for comparison, e.g.
// "NVIDIA H100", "h100" and "H100-SXM5" all reduce to "H100".
func NormalizeGPUType(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	for _, drop := range []string{"NVIDIA", " ", "-", "(", ")"} {
		s = strings.ReplaceAll(s, drop, "")
	}
	// Trim capacity suffixes before form-factor ones so a combined suffix like
	// "H100SXM580GB" reduces fully (80GB -> SXM5 -> H100).
	for _, suffix := range []string{"80GB", "40GB", "SXM5", "SXM4", "PCIE"} {
		s = strings.TrimSuffix(s, suffix)
	}
	return s
}

// SelectInstanceType picks a concrete Lambda instance type and region for the
// requested accelerator, matched purely against live capacity in entries.
//
//   - gpuType: preferred accelerator (e.g. "H100"); empty means "any with the
//     requested GPU count".
//   - count: GPUs per instance.
//   - preferredRegion: hard region preference; empty means "any region that
//     currently has capacity", and the cheapest match wins.
//
// It returns the chosen instance type name and region, or an error if nothing
// is available.
func SelectInstanceType(
	entries map[string]InstanceTypeEntry, gpuType string, count int, preferredRegion string,
) (name, region string, err error) {
	if count <= 0 {
		return "", "", fmt.Errorf("gpusPerReplica must be > 0")
	}

	bestName, bestRegion, bestPrice := "", "", int(^uint(0)>>1)
	for typeName, entry := range entries {
		if entry.InstanceType.Specs.GPUs != count {
			continue
		}
		if gpuType != "" && !instanceMatchesGPU(typeName, entry.InstanceType, gpuType) {
			continue
		}
		chosenRegion := pickRegion(entry.RegionsWithCapacityAvailable, preferredRegion)
		if chosenRegion == "" {
			continue
		}
		if price := entry.InstanceType.PriceCentsPerHour; price < bestPrice {
			bestName, bestRegion, bestPrice = typeName, chosenRegion, price
		}
	}

	if bestName == "" {
		if preferredRegion != "" {
			return "", "", fmt.Errorf("no Lambda capacity for %s x%d in region %q", gpuType, count, preferredRegion)
		}
		return "", "", fmt.Errorf("no Lambda capacity for %s x%d in any region", gpuType, count)
	}
	return bestName, bestRegion, nil
}

// instanceMatchesGPU reports whether an instance type is the requested
// accelerator. Matching is token-based on the instance type name (and the GPU
// description as a fallback) so that "A10" never matches "..._a100_..." — the
// substring trap that motivated the previous static lookup table.
func instanceMatchesGPU(typeName string, it InstanceType, gpuType string) bool {
	want := NormalizeGPUType(gpuType)
	if want == "" {
		return true
	}
	for _, token := range strings.FieldsFunc(typeName, func(r rune) bool { return r == '_' || r == '-' || r == ' ' }) {
		if NormalizeGPUType(token) == want {
			return true
		}
	}
	for _, token := range strings.FieldsFunc(it.GPUDescription, func(r rune) bool { return r == ' ' || r == '-' }) {
		if NormalizeGPUType(token) == want {
			return true
		}
	}
	return false
}

// pickRegion returns preferredRegion if it has capacity, otherwise the first
// available region. Returns "" when no region has capacity.
func pickRegion(available []Region, preferredRegion string) string {
	if len(available) == 0 {
		return ""
	}
	if preferredRegion == "" {
		return available[0].Name
	}
	for _, r := range available {
		if r.Name == preferredRegion {
			return preferredRegion
		}
	}
	return ""
}
