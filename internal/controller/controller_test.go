package controller

import (
	"testing"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/quota"
)

func TestAssignPrioritiesOrdersDistinctResets(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Quota: quota.Result{ResetAt: base.Add(24 * time.Hour)}},
		{Quota: quota.Result{ResetAt: base.Add(48 * time.Hour)}},
		{Quota: quota.Result{ResetAt: base.Add(72 * time.Hour)}},
	}
	got := AssignPriorities(items, 100, 5)
	want := []int{100, 95, 90}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestAssignPrioritiesKeepsExactResetTiesTogether(t *testing.T) {
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	items := []observation{
		{Quota: quota.Result{ResetAt: base}},
		{Quota: quota.Result{ResetAt: base}},
		{Quota: quota.Result{ResetAt: base.Add(time.Hour)}},
	}
	got := AssignPriorities(items, 100, 1)
	want := []int{100, 100, 99}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}
