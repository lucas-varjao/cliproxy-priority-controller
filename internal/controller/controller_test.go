package controller

import (
	"context"
	"testing"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/cliproxy"
	"github.com/lucas-varjao/cliproxy-priority-controller/internal/quota"
)

func TestAssignPrioritiesOrdersDistinctResets(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(24 * time.Hour)}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(48 * time.Hour)}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(72 * time.Hour)}}},
	}
	assertPriorities(t, AssignPriorities(items, 100, 5, FiveHourModeOff, 90), []int{100, 95, 90})
}

func TestAssignPrioritiesKeepsExactResetTiesTogether(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(time.Hour)}}},
	}
	assertPriorities(t, AssignPriorities(items, 100, 1, FiveHourModeOff, 90), []int{100, 100, 99})
}

func TestGuardrailMovesConstrainedAccountBehindHealthyAccount(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Credential: cliproxy.Credential{Name: "soon-but-hot"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(24 * time.Hour)}, FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 95}}},
		{Credential: cliproxy.Credential{Name: "later-but-healthy"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(48 * time.Hour)}, FiveHour: &quota.Window{ResetAt: base.Add(2 * time.Hour), Utilization: 20}}},
	}
	sortObservations(items, FiveHourModeGuardrail, 90)
	if got := items[0].Credential.TargetName(); got != "later-but-healthy" {
		t.Fatalf("first credential = %q", got)
	}
	assertPriorities(t, AssignPriorities(items, 100, 1, FiveHourModeGuardrail, 90), []int{100, 99})
}

func TestGuardrailSplitsSameWeeklyResetIntoDifferentPriorityTiers(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Credential: cliproxy.Credential{Name: "healthy"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base}, FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 30}}},
		{Credential: cliproxy.Credential{Name: "constrained"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base}, FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 90}}},
	}
	sortObservations(items, FiveHourModeGuardrail, 90)
	assertPriorities(t, AssignPriorities(items, 100, 1, FiveHourModeGuardrail, 90), []int{100, 99})
}

func TestFiveHourOffKeepsWeeklyOrdering(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Credential: cliproxy.Credential{Name: "soon-and-hot"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(24 * time.Hour)}, FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 99}}},
		{Credential: cliproxy.Credential{Name: "later"}, Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(48 * time.Hour)}, FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 10}}},
	}
	sortObservations(items, FiveHourModeOff, 90)
	if got := items[0].Credential.TargetName(); got != "soon-and-hot" {
		t.Fatalf("first credential = %q", got)
	}
}

func TestNextQuotaCheckUsesAdaptiveFiveHourIntervals(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	weekly := quota.Window{ResetAt: now.Add(48 * time.Hour)}
	cases := []struct {
		name string
		used float64
		want time.Duration
	}{
		{"low", 20, 30 * time.Minute},
		{"mid", 75, 15 * time.Minute},
		{"high", 87, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := quota.Result{Weekly: weekly, FiveHour: &quota.Window{ResetAt: now.Add(2 * time.Hour), Utilization: tc.used}}
			got := nextQuotaCheck(now, result, FiveHourModeGuardrail, 90)
			if want := now.Add(tc.want); !got.Equal(want) {
				t.Fatalf("next = %s, want %s", got, want)
			}
		})
	}
}

func TestNextQuotaCheckOffModeUsesLowFrequencyPoll(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	result := quota.Result{Weekly: quota.Window{ResetAt: now.Add(48 * time.Hour)}, FiveHour: &quota.Window{ResetAt: now.Add(2 * time.Hour), Utilization: 99}}
	got := nextQuotaCheck(now, result, FiveHourModeOff, 90)
	want := now.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextQuotaCheckWaitsForFiveHourResetWhenConstrained(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	result := quota.Result{Weekly: quota.Window{ResetAt: now.Add(48 * time.Hour)}, FiveHour: &quota.Window{ResetAt: reset, Utilization: 95}}
	got := nextQuotaCheck(now, result, FiveHourModeGuardrail, 90)
	want := reset.Add(quotaResetGrace)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextQuotaCheckWakesAfterWeeklyReset(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(4 * time.Minute)
	result := quota.Result{Weekly: quota.Window{ResetAt: weeklyReset}, FiveHour: &quota.Window{ResetAt: now.Add(2 * time.Hour), Utilization: 10}}
	got := nextQuotaCheck(now, result, FiveHourModeGuardrail, 90)
	want := weeklyReset.Add(quotaResetGrace)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestQuotaForCredentialUsesFreshCache(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	credential := cliproxy.Credential{Provider: "claude", AuthIndex: "auth-1", Name: "one.json"}
	cachedResult := quota.Result{Weekly: quota.Window{ResetAt: now.Add(24 * time.Hour), Utilization: 20}}
	c := &Controller{
		now: func() time.Time { return now },
		quotaCache: map[string]quotaCacheEntry{
			credentialCacheKey(credential): {Result: cachedResult, HasResult: true, NextCheck: now.Add(20 * time.Minute)},
		},
	}
	result, cached, nextCheck, err := c.quotaForCredential(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if !cached {
		t.Fatal("cached = false, want true")
	}
	if result.Weekly.Utilization != 20 {
		t.Fatalf("utilization = %v, want 20", result.Weekly.Utilization)
	}
	if !nextCheck.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("nextCheck = %s", nextCheck)
	}
}

func TestProviderBackoffStopsProviderBeforePolling(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	credential := cliproxy.Credential{Provider: "claude", AuthIndex: "auth-1", Name: "one.json"}
	c := &Controller{
		now: func() time.Time { return now },
		quotaCache: map[string]quotaCacheEntry{
			credentialCacheKey(credential): {RateLimited: true, NextCheck: now.Add(15 * time.Minute), Backoff: 15 * time.Minute},
		},
	}
	got := c.providerBackoffUntil([]cliproxy.Credential{credential})
	if want := now.Add(15 * time.Minute); !got.Equal(want) {
		t.Fatalf("backoff until = %s, want %s", got, want)
	}
}

func TestRateLimitBackoffDoublesAndCaps(t *testing.T) {
	if got := nextRateLimitBackoff(0); got != 15*time.Minute {
		t.Fatalf("first = %s", got)
	}
	if got := nextRateLimitBackoff(15 * time.Minute); got != 30*time.Minute {
		t.Fatalf("second = %s", got)
	}
	if got := nextRateLimitBackoff(90 * time.Minute); got != 2*time.Hour {
		t.Fatalf("cap = %s", got)
	}
	if got := nextRateLimitBackoff(2 * time.Hour); got != 2*time.Hour {
		t.Fatalf("cap repeat = %s", got)
	}
}

func assertPriorities(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}
