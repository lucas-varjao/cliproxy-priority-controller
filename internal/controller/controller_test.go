package controller

import (
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
	got := AssignPriorities(items, 100, 5, FiveHourModeOff, 90)
	want := []int{100, 95, 90}
	assertPriorities(t, got, want)
}

func TestAssignPrioritiesKeepsExactResetTiesTogether(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base}}},
		{Quota: quota.Result{Weekly: quota.Window{ResetAt: base.Add(time.Hour)}}},
	}
	got := AssignPriorities(items, 100, 1, FiveHourModeOff, 90)
	want := []int{100, 100, 99}
	assertPriorities(t, got, want)
}

func TestGuardrailMovesConstrainedAccountBehindHealthyAccount(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{
			Credential: cliproxy.Credential{Name: "soon-but-hot"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base.Add(24 * time.Hour)},
				FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 95},
			},
		},
		{
			Credential: cliproxy.Credential{Name: "later-but-healthy"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base.Add(48 * time.Hour)},
				FiveHour: &quota.Window{ResetAt: base.Add(2 * time.Hour), Utilization: 20},
			},
		},
	}

	sortObservations(items, FiveHourModeGuardrail, 90)
	if got := items[0].Credential.TargetName(); got != "later-but-healthy" {
		t.Fatalf("first credential = %q, want later-but-healthy", got)
	}
	got := AssignPriorities(items, 100, 1, FiveHourModeGuardrail, 90)
	assertPriorities(t, got, []int{100, 99})
}

func TestGuardrailSplitsSameWeeklyResetIntoDifferentPriorityTiers(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{
			Credential: cliproxy.Credential{Name: "healthy"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base},
				FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 30},
			},
		},
		{
			Credential: cliproxy.Credential{Name: "constrained"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base},
				FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 90},
			},
		},
	}

	sortObservations(items, FiveHourModeGuardrail, 90)
	got := AssignPriorities(items, 100, 1, FiveHourModeGuardrail, 90)
	assertPriorities(t, got, []int{100, 99})
}

func TestFiveHourOffKeepsWeeklyOrdering(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{
			Credential: cliproxy.Credential{Name: "soon-and-hot"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base.Add(24 * time.Hour)},
				FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 99},
			},
		},
		{
			Credential: cliproxy.Credential{Name: "later"},
			Quota: quota.Result{
				Weekly:   quota.Window{ResetAt: base.Add(48 * time.Hour)},
				FiveHour: &quota.Window{ResetAt: base.Add(time.Hour), Utilization: 10},
			},
		},
	}

	sortObservations(items, FiveHourModeOff, 90)
	if got := items[0].Credential.TargetName(); got != "soon-and-hot" {
		t.Fatalf("first credential = %q, want soon-and-hot", got)
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
