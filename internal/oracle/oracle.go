package oracle

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"waitlist-chaos/internal/domain"
	"waitlist-chaos/internal/store"
)

type CheckID string

const (
	Capacity             CheckID = "S-1"
	NoDuplicate          CheckID = "S-2"
	ConvertedXorRefunded CheckID = "S-3"
	FIFO                 CheckID = "S-4"
	SingleRefund         CheckID = "S-5"
	Resolution           CheckID = "L-1"
)

type Status string

const (
	Pass            Status = "PASS"
	Violation       Status = "VIOLATION"
	NotApplicable   Status = "NOT_APPLICABLE"
	NotYetJudgeable Status = "NOT_YET_JUDGEABLE"
)

type Overall string

const (
	OverallPass      Overall = "PASS"
	OverallSafety    Overall = "SAFETY_VIOLATION"
	OverallLiveness  Overall = "LIVENESS_VIOLATION"
	OverallNotJudged Overall = "NOT_YET_JUDGEABLE"
)

type Result struct {
	ID      CheckID  `json:"id"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Status  Status   `json:"status"`
	Details []string `json:"details,omitempty"`
}

type Verdict struct {
	EventID string   `json:"event_id"`
	Overall Overall  `json:"overall"`
	Results []Result `json:"results"`
}

// Violations returns IDs of violated checks in order.
func (v Verdict) Violations() []CheckID {
	var out []CheckID
	for _, r := range v.Results {
		if r.Status == Violation {
			out = append(out, r.ID)
		}
	}
	return out
}

type Event struct {
	ID          string
	Capacity    int
	DeadlineAt  time.Time
	Processed   bool
	ProcessedAt *time.Time
}

type Allocation struct {
	ID            string
	UserID        string
	Kind          string
	Status        string
	PurchasedAt   time.Time
	ConvertedFrom string
}

type Refund struct {
	AllocationID string
	Status       string
}

type Snapshot struct {
	Now         time.Time
	Event       Event
	Allocations []Allocation
	Refunds     []Refund
}

// Check loads a snapshot and returns its verdict.
func Check(ctx context.Context, q store.DBTX, eventID string, grace time.Duration) (Verdict, error) {
	s, err := Load(ctx, q, eventID)
	if err != nil {
		return Verdict{}, err
	}
	return Evaluate(s, grace), nil
}

func Load(ctx context.Context, q store.DBTX, eventID string) (Snapshot, error) {
	var s Snapshot
	e := &s.Event
	err := q.QueryRow(ctx, `
		SELECT id::text, confirmed_capacity, deadline_at, deadline_processed, deadline_processed_at, now()
		FROM events WHERE id = $1`, eventID).
		Scan(&e.ID, &e.Capacity, &e.DeadlineAt, &e.Processed, &e.ProcessedAt, &s.Now)
	if err != nil {
		return s, fmt.Errorf("load event %s: %w", eventID, err)
	}

	rows, err := q.Query(ctx, `
		SELECT id::text, user_id, kind, status, purchased_at, COALESCE(converted_from::text, '')
		FROM allocations WHERE event_id = $1`, eventID)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var a Allocation
		if err := rows.Scan(&a.ID, &a.UserID, &a.Kind, &a.Status, &a.PurchasedAt, &a.ConvertedFrom); err != nil {
			rows.Close()
			return s, err
		}
		s.Allocations = append(s.Allocations, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}

	rows, err = q.Query(ctx, `
		SELECT r.allocation_id::text, r.status
		FROM refund_ledger r JOIN allocations a ON a.id = r.allocation_id
		WHERE a.event_id = $1`, eventID)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Refund
		if err := rows.Scan(&r.AllocationID, &r.Status); err != nil {
			return s, err
		}
		s.Refunds = append(s.Refunds, r)
	}
	return s, rows.Err()
}

type facts struct {
	waitlist         map[string]Allocation
	converted        map[string]bool
	refunded         map[string]bool
	completedRefunds map[string]int
	pendingRefunds   int
	activeConfirmed  []Allocation
}

func derive(s Snapshot) facts {
	f := facts{
		waitlist:         map[string]Allocation{},
		converted:        map[string]bool{},
		refunded:         map[string]bool{},
		completedRefunds: map[string]int{},
	}
	for _, a := range s.Allocations {
		switch a.Kind {
		case store.KindWaitlist:
			f.waitlist[a.ID] = a
			switch a.Status {
			case "converted":
				f.converted[a.ID] = true
			case "refunded":
				f.refunded[a.ID] = true
			}
		case store.KindConfirmed:
			if a.ConvertedFrom != "" {
				f.converted[a.ConvertedFrom] = true
			}
			if a.Status == "active" {
				f.activeConfirmed = append(f.activeConfirmed, a)
			}
		}
	}
	for _, r := range s.Refunds {
		switch r.Status {
		case "completed":
			f.completedRefunds[r.AllocationID]++
			f.refunded[r.AllocationID] = true
		case "pending":
			f.pendingRefunds++
		}
	}
	return f
}

func (f facts) user(id string) string {
	if a, ok := f.waitlist[id]; ok {
		return a.UserID
	}
	return id
}

func (f facts) entry(id string) domain.Entry {
	a := f.waitlist[id]
	return domain.Entry{ID: a.ID, UserID: a.UserID, PurchasedAt: a.PurchasedAt}
}

// Evaluate checks all invariants against a snapshot.
func Evaluate(s Snapshot, grace time.Duration) Verdict {
	f := derive(s)
	results := []Result{
		checkCapacity(s, f),
		checkNoDuplicate(f),
		checkConvertedXorRefunded(f),
		checkFIFO(f),
		checkSingleRefund(f),
		checkResolution(s, f, grace),
	}
	v := Verdict{EventID: s.Event.ID, Overall: OverallPass, Results: results}
	var safety, liveness, pending bool
	for _, r := range results {
		switch {
		case r.Status == Violation && r.Kind == "safety":
			safety = true
		case r.Status == Violation:
			liveness = true
		case r.Status == NotYetJudgeable:
			pending = true
		}
	}
	switch {
	case safety:
		v.Overall = OverallSafety
	case liveness:
		v.Overall = OverallLiveness
	case pending:
		v.Overall = OverallNotJudged
	}
	return v
}

func result(id CheckID, kind, name string, details []string) Result {
	r := Result{ID: id, Kind: kind, Name: name, Status: Pass}
	if len(details) > 0 {
		slices.Sort(details)
		r.Status, r.Details = Violation, details
	}
	return r
}

func checkCapacity(s Snapshot, f facts) Result {
	var details []string
	if n := len(f.activeConfirmed); n > s.Event.Capacity {
		details = append(details, fmt.Sprintf("%d active confirmed seats exceed capacity %d", n, s.Event.Capacity))
	}
	return result(Capacity, "safety", "active confirmed seats <= capacity", details)
}

func checkNoDuplicate(f facts) Result {
	perUser := map[string]int{}
	for _, a := range f.activeConfirmed {
		perUser[a.UserID]++
	}
	var details []string
	for u, n := range perUser {
		if n > 1 {
			details = append(details, fmt.Sprintf("%s holds %d active confirmed seats", u, n))
		}
	}
	return result(NoDuplicate, "safety", "at most one active confirmed seat per user", details)
}

func checkConvertedXorRefunded(f facts) Result {
	var details []string
	for id := range f.converted {
		if f.refunded[id] {
			details = append(details, fmt.Sprintf("%s received a confirmed seat AND a refund", f.user(id)))
		}
	}
	return result(ConvertedXorRefunded, "safety", "a waitlist entry is converted or refunded, never both", details)
}

func checkFIFO(f facts) Result {
	var details []string
	for r := range f.refunded {
		for c := range f.converted {
			if c != r && domain.FIFOLess(f.entry(r), f.entry(c)) {
				details = append(details, fmt.Sprintf("%s was refunded although later entry %s was converted", f.user(r), f.user(c)))
			}
		}
	}
	return result(FIFO, "safety", "conversions follow FIFO order", details)
}

func checkSingleRefund(f facts) Result {
	var details []string
	for id, n := range f.completedRefunds {
		if n > 1 {
			details = append(details, fmt.Sprintf("%s was refunded %d times", f.user(id), n))
		}
	}
	return result(SingleRefund, "safety", "at most one completed refund per entry", details)
}

func checkResolution(s Snapshot, f facts, grace time.Duration) Result {
	const name = "every waitlist entry is resolved after the deadline"
	r := Result{ID: Resolution, Kind: "liveness", Name: name}
	ev := s.Event
	if !ev.Processed {
		switch {
		case s.Now.Before(ev.DeadlineAt):
			r.Status, r.Details = NotApplicable, []string{"deadline not reached yet"}
		case s.Now.Before(ev.DeadlineAt.Add(grace)):
			r.Status, r.Details = NotYetJudgeable, []string{"deadline passed, finalisation still within grace period"}
		default:
			r.Status, r.Details = Violation, []string{fmt.Sprintf("deadline passed at %s but the event was never finalised", ev.DeadlineAt.Format(time.RFC3339))}
		}
		return r
	}
	if ev.ProcessedAt != nil && s.Now.Before(ev.ProcessedAt.Add(grace)) {
		r.Status, r.Details = NotYetJudgeable, []string{"finalised recently, still within grace period"}
		return r
	}
	var details, stuck []string
	for id, a := range f.waitlist {
		if a.Status == "active" {
			stuck = append(stuck, f.user(id))
		}
	}
	if len(stuck) > 0 {
		slices.Sort(stuck)
		details = append(details, fmt.Sprintf("%d waitlist entries still unresolved (no seat, no refund): %s", len(stuck), strings.Join(stuck, " ")))
	}
	for id, a := range f.waitlist {
		if a.Status == "refunded" && f.completedRefunds[id] == 0 {
			details = append(details, fmt.Sprintf("%s is marked refunded but has no completed refund", f.user(id)))
		}
	}
	if f.pendingRefunds > 0 {
		details = append(details, fmt.Sprintf("%d refunds still pending", f.pendingRefunds))
	}
	return result(Resolution, "liveness", name, details)
}

// Print writes a human-readable verdict to w.
func Print(w io.Writer, v Verdict) {
	fmt.Fprintf(w, "event %s: %s\n", v.EventID, v.Overall)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range v.Results {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.ID, r.Status, r.Name)
		for _, d := range r.Details {
			fmt.Fprintf(tw, "  \t\t- %s\n", d)
		}
	}
	tw.Flush()
}
