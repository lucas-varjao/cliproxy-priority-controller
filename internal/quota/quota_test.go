package quota

import (
	"testing"
	"time"
)

func TestParseClaudeSevenDay(t *testing.T) {
	result, err := parseClaude([]byte(`{"five_hour":{"utilization":12,"resets_at":"2026-10-12T23:00:00Z"},"seven_day":{"utilization":80,"resets_at":"2026-10-18T21:59:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.ResetAt.Format(time.RFC3339), "2026-10-18T21:59:00Z"; got != want {
		t.Fatalf("reset = %s, want %s", got, want)
	}
	if result.Utilization != 80 {
		t.Fatalf("utilization = %v, want 80", result.Utilization)
	}
}

func TestParseClaudeLimitsFallback(t *testing.T) {
	result, err := parseClaude([]byte(`{"seven_day":null,"limits":[{"kind":"weekly_all","percent":42,"resets_at":"2026-10-15T12:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.ResetAt.Format(time.RFC3339), "2026-10-15T12:00:00Z"; got != want {
		t.Fatalf("reset = %s, want %s", got, want)
	}
}

func TestParseCodexSecondaryWeekly(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	result, err := parseCodex([]byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 25, "limit_window_seconds": 18000, "reset_at": 1791849600},
			"secondary_window": {"used_percent": 70, "limit_window_seconds": 604800, "reset_at": 1792278000}
		}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Utilization != 70 {
		t.Fatalf("utilization = %v, want 70", result.Utilization)
	}
	if result.Source != "rate_limit:secondary_window" {
		t.Fatalf("source = %q", result.Source)
	}
}

func TestParseCodexWeeklyCanBePrimary(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	result, err := parseCodex([]byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 30, "limit_window_seconds": 604800, "reset_after_seconds": 3600}
		}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(time.Hour)
	if !result.ResetAt.Equal(want) {
		t.Fatalf("reset = %s, want %s", result.ResetAt, want)
	}
}

func TestParseCodexDoesNotTreatMonthlyAsWeekly(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	_, err := parseCodex([]byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 30, "limit_window_seconds": 2592000, "reset_after_seconds": 3600}
		}
	}`), now)
	if err == nil {
		t.Fatal("expected error for monthly-only response")
	}
}
