package benchmark

import (
	"testing"
	"time"
)

func TestFormatScenarioRunIDUsesUTC(t *testing.T) {
	localTime := time.Date(2026, 5, 11, 15, 4, 5, 0, time.FixedZone("PDT", -7*3600))

	runID := formatScenarioRunID(localTime, "AIBrix Hello World")

	const want = "20260511-220405-UTC-aibrix-hello-world"
	if runID != want {
		t.Fatalf("formatScenarioRunID() = %q, want %q", runID, want)
	}
}

func TestNowInUTCUsesUTCLocation(t *testing.T) {
	if got := nowInUTC().Location(); got != time.UTC {
		t.Fatalf("nowInUTC() location = %v, want %v", got, time.UTC)
	}
}
