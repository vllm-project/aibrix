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
