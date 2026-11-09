package health

import (
	"context"
	"fmt"
	"slices"

	"waitlist-chaos/internal/store"
)

type Status string

const (
	Green Status = "GREEN"
	Red   Status = "RED"
)

type Report struct {
	Status           Status
	Reasons          []string
	CrashesRecovered int
	FinalExitCodes   map[string]int
	Jobs             map[string]int
}

// Check returns GREEN when all workers exited cleanly and no jobs remain; recovered crashes don't affect status.
func Check(ctx context.Context, q store.DBTX, eventID string, finalExit map[string]int, crashesRecovered int) (Report, error) {
	jobs, err := store.JobCounts(ctx, q, eventID)
	if err != nil {
		return Report{}, err
	}
	r := Report{Status: Green, CrashesRecovered: crashesRecovered, FinalExitCodes: finalExit, Jobs: jobs}

	names := make([]string, 0, len(finalExit))
	for n := range finalExit {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if c := finalExit[n]; c != 0 {
			r.Reasons = append(r.Reasons, fmt.Sprintf("%s exited with code %d", n, c))
		}
	}
	for _, s := range []string{"pending", "processing", "failed"} {
		if jobs[s] > 0 {
			r.Reasons = append(r.Reasons, fmt.Sprintf("%d conversion jobs %s", jobs[s], s))
		}
	}
	if len(r.Reasons) > 0 {
		r.Status = Red
	}
	return r, nil
}
