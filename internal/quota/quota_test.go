package quota

import (
	"testing"
	"time"
)

func TestParseClaudeReturnsFiveHourAndWeekly(t *testing.T) {
	result, err := parseClaude([]byte(`{"five_hour":{"utilization":12,"resets_at":"2026-10-12T23:00:00Z"},"seven_day":{"utilization":80,"resets_at":"2026-10-18T21:59:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.FiveHour == nil {
		t.Fatal("five-hour window is nil")
	}
	if got, want := result.FiveHour.ResetAt.Format(time.RFC3339), "2026-10-12T23:00:00Z"; got != want {
		t.Fatalf("five-hour reset = %s, want %s", got, want)
	}
	if result.FiveHour.Utilization != 12 {
		t.Fatalf("five-hour utilization = %v, want 12", result.FiveHour.Utilization)
	}
	if got, want := result.Weekly.ResetAt.Format(time.RFC3339), "2026-10-18T21:59:00Z"; got != want {
		t.Fatalf("weekly reset = %s, want %s", got, want)
	}
	if result.Weekly.Utilization != 80 {
		t.Fatalf("weekly utilization = %v, want 80", result.Weekly.Utilization)
	}
}

func TestParseClaudeLimitsFallbackReturnsBothWindows(t *testing.T) {
	result, err := parseClaude([]byte(`{"five_hour":null,"seven_day":null,"limits":[{"kind":"session","percent":25,"resets_at":"2026-10-12T23:00:00Z"},{"kind":"weekly_all","percent":42,"resets_at":"2026-10-15T12:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.FiveHour == nil {
		t.Fatal("five-hour fallback window is nil")
	}
	if result.FiveHour .Source != "limits:session" {
		t.Fatalf("five-hour source = %q", result.FiveHour.Source)
	}
	if got, want := result.Weekly.ResetAt.Format(time.RFC3339), "2026-10-15T12:00:00Z"; got != want {
		t.Fatalf("weekly reset = %s, want %s", got, want)
	}
}

func TestParseCodexSecondaryWeeklyAndPrimaryFiveHour(t *testing.T) {
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
	if result.FiveHour == nil {
		t.Fatal("five-hour window is nil")
	}
	if result.FiveHour.Utilization != 25 {
		t.Fatalf("five-hour utilization = %v, want 25", result.FiveHour .Utilization)
	}
	if result.Weekly.Utilization != 70 {
		t.Fatalf("weekly utilization = %v, want 70", result.Weekly.Utilization)
	}
	if result.Weekly.Source != "rate_limit:secondary_window" {
		t.Fatalf("weekly source = %q", result.Weekly.Source)
	}
}

func TestParseCodexWeeklyCanBePrimary(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	result, err := parseCodex([]byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(time.Hour); !result.Weekly.ResetAt.Equal(want) {
		t.Fatalf("weekly reset = %s, want %s", result.Weekly.ResetAt, want)
	}
	if result.FiveHour != nil {
		t.Fatalf("five-hour window = %#v, want nil", result.FiveHour)
	}
}

func TestParseCodexLegacyPrimaryAndSecondary(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	result, err := parseCodex([]byte(`{"rate_limit":{"primary_window":{"used_percent":35,"reset_after_seconds":3600},"secondary_window":{"used_percent":65,"reset_after_seconds":86400}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.FiveHour == nil || result.FiveHour.Utilization != 35 {
		t.Fatalf("five-hour = %#v, want utilization 35", result.FiveHour)
	}
	if result.Weekly.Utilization != 65 {
		t.Fatalf("weekly utilization = %v, want 65", result.Weekly.Utilization)
	}
}

func TestParseCodexDoesNotTreatMonthlyAsWeekly(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	_, err := parseCodex([]byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":2592000,"reset_after_seconds":3600}}}`), now)
	if err == nil {
		t.Fatal("expected error for monthly-only response")
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	got := parseRetryAfter(map[string][]string{"retry-after": {"120"}}, now)
	if got != 2*time.Minute {
		t.Fatalf("retry-after = %s, want 2m", got)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	got := parseRetryAfter(map[string][]string{"Retry-After": {"Sun, 12 Oct 2026 20:10:00 GMT"}}, now)
	if got != 10*time.Minute {
		t.Fatalf("retry-after = %s, want 10m", got)
	}
}
