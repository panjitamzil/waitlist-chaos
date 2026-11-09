package domain

import (
	"testing"
	"time"
)

func ids(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlanDeadline(t *testing.T) {
	t0 := time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC)
	waitlist := []Entry{
		{ID: "c", PurchasedAt: t0.Add(2 * time.Minute)},
		{ID: "a", PurchasedAt: t0},
		{ID: "e", PurchasedAt: t0.Add(4 * time.Minute)},
		{ID: "b", PurchasedAt: t0.Add(time.Minute)},
		{ID: "d", PurchasedAt: t0.Add(3 * time.Minute)},
	}
	tests := []struct {
		name            string
		free            int
		convert, refund []string
	}{
		{"two free seats", 2, []string{"a", "b"}, []string{"c", "d", "e"}},
		{"no free seats", 0, []string{}, []string{"a", "b", "c", "d", "e"}},
		{"negative is treated as zero", -1, []string{}, []string{"a", "b", "c", "d", "e"}},
		{"more seats than entries", 9, []string{"a", "b", "c", "d", "e"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv, ref := PlanDeadline(tt.free, waitlist)
			if !equal(ids(conv), tt.convert) || !equal(ids(ref), tt.refund) {
				t.Fatalf("got convert=%v refund=%v, want %v %v", ids(conv), ids(ref), tt.convert, tt.refund)
			}
		})
	}
	if waitlist[0].ID != "c" {
		t.Fatal("PlanDeadline must not reorder its input")
	}
}

func TestFIFOTieBreakByID(t *testing.T) {
	t0 := time.Now()
	es := []Entry{{ID: "b", PurchasedAt: t0}, {ID: "a", PurchasedAt: t0}}
	SortFIFO(es)
	if !equal(ids(es), []string{"a", "b"}) {
		t.Fatalf("got %v", ids(es))
	}
}
