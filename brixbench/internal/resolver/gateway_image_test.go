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
	"context"
	"strings"
	"testing"
)

const (
	testGatewayImage  = "hub.byted.org/brixbench/aibrix.gateway.test:ci-9aa8b21ef605"
	testGatewayCommit = "9aa8b21ef6053dc19dde76f71552247b82f93630"
)

func TestPrepareGatewayImageUsesPrebuiltImageAndCommit(t *testing.T) {
	t.Setenv("BENCHMARK_GATEWAY_IMAGE", testGatewayImage)
	t.Setenv("BENCHMARK_GATEWAY_COMMIT", strings.ToUpper(testGatewayCommit))

	provider := "aibrix"
	testCase := &Test{
		Name:     "aibrix-pd-env-override",
		Provider: &provider,
		Version:  "v0.6.0",
	}

	got, err := PrepareGatewayImage(context.Background(), t.TempDir(), testCase)
	if err != nil {
		t.Fatalf("PrepareGatewayImage returned error: %v", err)
	}
	if got == nil {
		t.Fatal("expected gateway image, got nil")
	}
	if got.Image != testGatewayImage {
		t.Fatalf("Image = %q, want %q", got.Image, testGatewayImage)
	}
	if got.Repository != "hub.byted.org/brixbench/aibrix.gateway.test" {
		t.Fatalf("Repository = %q", got.Repository)
	}
	if got.Tag != "ci-9aa8b21ef605" {
		t.Fatalf("Tag = %q", got.Tag)
	}
	if testCase.ResolvedCommit != testGatewayCommit {
		t.Fatalf("ResolvedCommit = %q, want %q", testCase.ResolvedCommit, testGatewayCommit)
	}
	if testCase.GatewayImage != got.Image ||
		testCase.GatewayImageRepository != got.Repository ||
		testCase.GatewayImageTag != got.Tag {
		t.Fatalf(
			"test case fields not updated: image=%q repo=%q tag=%q",
			testCase.GatewayImage,
			testCase.GatewayImageRepository,
			testCase.GatewayImageTag,
		)
	}
}

func TestPrepareGatewayImagePreservesResolvedCommitWithoutCommitOverride(t *testing.T) {
	t.Setenv("BENCHMARK_GATEWAY_IMAGE", testGatewayImage)

	provider := "aibrix"
	testCase := &Test{
		Name:           "aibrix-pd-image-only",
		Provider:       &provider,
		ResolvedCommit: "52405d78",
	}

	if _, err := PrepareGatewayImage(context.Background(), t.TempDir(), testCase); err != nil {
		t.Fatalf("PrepareGatewayImage returned error: %v", err)
	}
	if testCase.ResolvedCommit != "52405d78" {
		t.Fatalf("ResolvedCommit = %q, want existing value", testCase.ResolvedCommit)
	}
}

func TestPrepareGatewayImageRejectsInvalidPrebuiltConfiguration(t *testing.T) {
	provider := "aibrix"

	tests := []struct {
		name   string
		image  string
		commit string
	}{
		{name: "invalid image", image: "not-a-valid-image-ref"},
		{name: "short commit", image: testGatewayImage, commit: "9aa8b21"},
		{name: "non-hex commit", image: testGatewayImage, commit: strings.Repeat("z", 40)},
		{name: "commit without image", commit: testGatewayCommit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BENCHMARK_GATEWAY_IMAGE", tt.image)
			t.Setenv("BENCHMARK_GATEWAY_COMMIT", tt.commit)

			_, err := PrepareGatewayImage(context.Background(), t.TempDir(), &Test{
				Name:     "aibrix-pd-invalid-override",
				Provider: &provider,
			})
			if err == nil {
				t.Fatal("expected invalid prebuilt gateway configuration to fail")
			}
		})
	}
}

func TestPrepareGatewayImageIgnoresOverrideForNonAIBrixProvider(t *testing.T) {
	t.Setenv("BENCHMARK_GATEWAY_IMAGE", "not-a-valid-image-ref")
	t.Setenv("BENCHMARK_GATEWAY_COMMIT", "not-a-valid-commit")

	provider := "dynamo"
	got, err := PrepareGatewayImage(context.Background(), t.TempDir(), &Test{
		Name:     "dynamo-case",
		Provider: &provider,
	})
	if err != nil {
		t.Fatalf("PrepareGatewayImage returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no AIBrix gateway image, got %#v", got)
	}
}

func TestPrepareGatewayImageWithoutOverrideKeepsExistingGuard(t *testing.T) {
	provider := "aibrix"
	got, err := PrepareGatewayImage(context.Background(), t.TempDir(), &Test{
		Name:     "aibrix-no-workspace",
		Provider: &provider,
	})
	if err != nil {
		t.Fatalf("PrepareGatewayImage returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no gateway image without a workspace, got %#v", got)
	}
}
