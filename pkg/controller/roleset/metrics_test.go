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

package roleset

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClassifyInPlaceFallbackReason asserts that every reason string
// currently produced by canInPlaceUpdatePod (pkg/controller/roleset/inplace_update.go)
// and canRolloutPodsInPlace/podset_rollsyncer.go's PodSet fallback maps to a
// non-"other" bucket, and that unrecognized free text - which may embed pod
// names - safely falls back to "other" instead of leaking as a label value.
func TestClassifyInPlaceFallbackReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "non-image pod fields changed",
			reason: "non-image pod fields changed",
			want:   "non_image_field_changed",
		},
		{
			name:   "no container image changes found",
			reason: "no container image changes found",
			want:   "no_image_diff",
		},
		{
			name:   "pod is already updated",
			reason: "pod is already updated",
			want:   "already_updated",
		},
		{
			name:   "init container image changes require pod recreation",
			reason: "init container image changes require pod recreation",
			want:   "init_container_changed",
		},
		{
			name:   "uses PodSet and cannot be updated in place",
			reason: fmt.Sprintf("role %s uses PodSet and cannot be updated in place", "decode"),
			want:   "podset_role",
		},
		{
			name:   "wrapped canRolloutPodsInPlace message still matches the embedded reason",
			reason: fmt.Sprintf("role %s pod %s cannot be updated in place: %s", "decode", "decode-0", "init container image changes require pod recreation"),
			want:   "init_container_changed",
		},
		{
			name:   "invalid role replica index falls back to other",
			reason: fmt.Sprintf("invalid role replica index %q", "abc"),
			want:   "other",
		},
		{
			name:   "empty reason falls back to other",
			reason: "",
			want:   "other",
		},
		{
			name:   "unknown free text embedding a pod name falls back to other",
			reason: "role decode pod decode-7f8b9c-xyz12 hit an unexpected error",
			want:   "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInPlaceFallbackReason(tt.reason)
			assert.Equal(t, tt.want, got)
			if tt.reason != "" && tt.want == "other" {
				return
			}
		})
	}
}

// TestClassifyInPlaceFallbackReasonNeverLeaksFreeText further guards against
// cardinality explosion: any reason containing a pod-name-like token that
// isn't one of the known substrings must classify to "other", never to a
// value derived from the free text itself.
func TestClassifyInPlaceFallbackReasonNeverLeaksFreeText(t *testing.T) {
	reason := "role decode pod decode-0123456789abcdef cannot be updated for an unforeseen reason"
	got := classifyInPlaceFallbackReason(reason)
	assert.Equal(t, "other", got)
	assert.NotContains(t, got, "decode-0123456789abcdef")
}
