package domain

import (
	"slices"
	"time"
)

type Entry struct {
	ID          string
	UserID      string
	PurchasedAt time.Time
}

// FIFOLess orders by purchase time, then by id as tiebreaker.
func FIFOLess(a, b Entry) bool {
	if !a.PurchasedAt.Equal(b.PurchasedAt) {
		return a.PurchasedAt.Before(b.PurchasedAt)
	}
	return a.ID < b.ID
}

func SortFIFO(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		switch {
		case FIFOLess(a, b):
			return -1
		case FIFOLess(b, a):
			return 1
		}
		return 0
	})
}

// PlanDeadline returns which entries to convert (first N by FIFO) and which to refund.
func PlanDeadline(freeSeats int, waitlist []Entry) (convert, refund []Entry) {
	sorted := slices.Clone(waitlist)
	SortFIFO(sorted)
	n := min(max(freeSeats, 0), len(sorted))
	return sorted[:n], sorted[n:]
}
