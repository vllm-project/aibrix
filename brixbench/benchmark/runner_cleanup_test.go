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

package benchmark

import "testing"

func TestCleanupAfterTestEnabledDefaultsToFlag(t *testing.T) {
	t.Setenv("BENCHMARK_CLEANUP_AFTER_TEST", "")

	if !cleanupAfterTestEnabled() {
		t.Fatalf("expected cleanup to be enabled by default")
	}
}

func TestCleanupAfterTestEnabledUsesEnvOverride(t *testing.T) {
	t.Setenv("BENCHMARK_CLEANUP_AFTER_TEST", "false")

	if cleanupAfterTestEnabled() {
		t.Fatalf("expected cleanup to be disabled by env override")
	}
}

func TestCleanupAfterTestEnabledAcceptsOff(t *testing.T) {
	t.Setenv("BENCHMARK_CLEANUP_AFTER_TEST", "off")

	if cleanupAfterTestEnabled() {
		t.Fatalf("expected cleanup to be disabled by off env override")
	}
}

func TestResetBeforeTestEnabledDefaultsToFlag(t *testing.T) {
	t.Setenv("BENCHMARK_RESET_BEFORE_TEST", "")

	if !resetBeforeTestEnabled() {
		t.Fatalf("expected reset to be enabled by default")
	}
}

func TestResetBeforeTestEnabledUsesEnvOverride(t *testing.T) {
	t.Setenv("BENCHMARK_RESET_BEFORE_TEST", "false")

	if resetBeforeTestEnabled() {
		t.Fatalf("expected reset to be disabled by env override")
	}
}

func TestResetBeforeTestEnabledAcceptsOff(t *testing.T) {
	t.Setenv("BENCHMARK_RESET_BEFORE_TEST", "off")

	if resetBeforeTestEnabled() {
		t.Fatalf("expected reset to be disabled by off env override")
	}
}

func TestPodMonitoringEnabledDefaultsToFlag(t *testing.T) {
	t.Setenv("BENCHMARK_POD_MONITORING", "")

	if !podMonitoringEnabled() {
		t.Fatalf("expected pod monitoring to be enabled by default")
	}
}

func TestPodMonitoringEnabledUsesEnvOverride(t *testing.T) {
	t.Setenv("BENCHMARK_POD_MONITORING", "false")

	if podMonitoringEnabled() {
		t.Fatalf("expected pod monitoring to be disabled by env override")
	}
}

func TestPodMonitoringEnabledAcceptsOff(t *testing.T) {
	t.Setenv("BENCHMARK_POD_MONITORING", "off")

	if podMonitoringEnabled() {
		t.Fatalf("expected pod monitoring to be disabled by off env override")
	}
}

func TestPodMonitoringStrictDefaultsFalse(t *testing.T) {
	t.Setenv("BENCHMARK_POD_MONITORING_STRICT", "")

	if podMonitoringStrictEnabled() {
		t.Fatalf("expected pod monitoring strict mode to be disabled by default")
	}
}

func TestPodMonitoringStrictUsesEnvOverride(t *testing.T) {
	t.Setenv("BENCHMARK_POD_MONITORING_STRICT", "true")

	if !podMonitoringStrictEnabled() {
		t.Fatalf("expected pod monitoring strict mode to be enabled by env override")
	}
}
