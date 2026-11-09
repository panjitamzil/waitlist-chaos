package oracle

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 12, 3, 9, 0, 0, 0, time.UTC)

// fixture: capacity 5, three confirmed holders, waitlist w1..w5 in FIFO order,
// deadline at t0.
func fixture() Snapshot {
	s := Snapshot{
		Now:   t0.Add(time.Minute),
		Event: Event{ID: "ev", Capacity: 5, DeadlineAt: t0},
	}
	for i := 1; i <= 3; i++ {
		s.Allocations = append(s.Allocations, Allocation{ID: fmt.Sprintf("c%d", i), UserID: fmt.Sprintf("c%d", i), Kind: "confirmed", Status: "active", PurchasedAt: t0.Add(-time.Hour)})
	}
	for i := 1; i <= 5; i++ {
		s.Allocations = append(s.Allocations, Allocation{ID: fmt.Sprintf("w%d", i), UserID: fmt.Sprintf("w%d", i), Kind: "waitlist", Status: "active", PurchasedAt: t0.Add(time.Duration(i-30) * time.Minute)})
	}
	return s
}

func (s *Snapshot) set(id, status string) {
	for i := range s.Allocations {
		if s.Allocations[i].ID == id {
			s.Allocations[i].Status = status
		}
	}
}

func (s *Snapshot) convert(waitlistID string) {
	s.set(waitlistID, "converted")
	s.Allocations = append(s.Allocations, Allocation{ID: "conf-" + waitlistID + fmt.Sprint(len(s.Allocations)), UserID: waitlistID, Kind: "confirmed", Status: "active", PurchasedAt: s.Now, ConvertedFrom: waitlistID})
}

func (s *Snapshot) refund(id string) {
	s.set(id, "refunded")
	s.Refunds = append(s.Refunds, Refund{AllocationID: id, Status: "completed"})
}

func (s *Snapshot) finalise() {
	at := t0
	s.Event.Processed, s.Event.ProcessedAt = true, &at
}

// correct deadline outcome: w1,w2 converted, w3..w5 refunded.
func correct() Snapshot {
	s := fixture()
	s.convert("w1")
	s.convert("w2")
	s.refund("w3")
	s.refund("w4")
	s.refund("w5")
	s.finalise()
	return s
}

func assertVerdict(t *testing.T, v Verdict, overall Overall, violations ...CheckID) {
	t.Helper()
	if v.Overall != overall || !slices.Equal(v.Violations(), violations) {
		t.Fatalf("got %s %v, want %s %v\n%+v", v.Overall, v.Violations(), overall, violations, v.Results)
	}
}

func TestCorrectOutcomePasses(t *testing.T) {
	assertVerdict(t, Evaluate(correct(), 0), OverallPass)
}

func TestCapacityExceeded(t *testing.T) {
	s := correct()
	s.convert("w3")
	assertVerdict(t, Evaluate(s, 0), OverallSafety, Capacity, ConvertedXorRefunded)
}

func TestDuplicateConfirmed(t *testing.T) {
	s := correct()
	s.Allocations = append(s.Allocations, Allocation{ID: "dup", UserID: "c1", Kind: "confirmed", Status: "active"})
	s.Event.Capacity = 10
	assertVerdict(t, Evaluate(s, 0), OverallSafety, NoDuplicate)
}

func TestConvertedAndRefunded(t *testing.T) {
	s := fixture()
	s.convert("w1")
	s.convert("w2")
	s.refund("w2") // unguarded refund overwrites the converted status
	s.refund("w3")
	s.refund("w4")
	s.refund("w5")
	s.finalise()
	assertVerdict(t, Evaluate(s, 0), OverallSafety, ConvertedXorRefunded)
}

func TestFIFOViolation(t *testing.T) {
	s := fixture()
	s.convert("w1")
	s.convert("w3")
	s.refund("w2")
	s.refund("w4")
	s.refund("w5")
	s.finalise()
	assertVerdict(t, Evaluate(s, 0), OverallSafety, FIFO)
}

func TestDoubleRefund(t *testing.T) {
	s := correct()
	s.Refunds = append(s.Refunds, Refund{AllocationID: "w5", Status: "completed"})
	assertVerdict(t, Evaluate(s, 0), OverallSafety, SingleRefund)
}

func TestUnresolvedAfterFinalisation(t *testing.T) {
	s := fixture()
	s.finalise()
	assertVerdict(t, Evaluate(s, 0), OverallLiveness, Resolution)
}

func TestDeadlineNotReachedIsNotJudged(t *testing.T) {
	s := fixture()
	s.Now = t0.Add(-time.Hour)
	v := Evaluate(s, 0)
	assertVerdict(t, v, OverallPass)
	if v.Results[5].Status != NotApplicable {
		t.Fatalf("L-1 status = %s", v.Results[5].Status)
	}
}

func TestWithinGraceIsNotYetJudgeable(t *testing.T) {
	s := fixture()
	s.Now = t0.Add(time.Second)
	assertVerdict(t, Evaluate(s, 5*time.Second), OverallNotJudged)

	s.finalise()
	assertVerdict(t, Evaluate(s, 5*time.Second), OverallNotJudged)
}

func TestDeadlinePassedButNeverFinalised(t *testing.T) {
	s := fixture()
	s.Now = t0.Add(time.Minute)
	assertVerdict(t, Evaluate(s, 5*time.Second), OverallLiveness, Resolution)
}

func TestRefundedWithoutLedgerEntry(t *testing.T) {
	s := correct()
	s.Refunds = s.Refunds[:2]
	assertVerdict(t, Evaluate(s, 0), OverallLiveness, Resolution)
}
