//go:build integration

package worker_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/fault"
	"waitlist-chaos/internal/oracle"
	"waitlist-chaos/internal/store"
	"waitlist-chaos/internal/worker"
)

type fixture struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	eventID   string
	confirmed []string
}

func setup(t *testing.T, capacity, confirmed, waitlist int, deadlineIn time.Duration) *fixture {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("no DATABASE_URL: %v", err)
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	f := &fixture{ctx: ctx, pool: pool}
	if f.eventID, err = store.CreateEvent(ctx, pool, t.Name(), "worker-test", "test", capacity, deadlineIn); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= confirmed; i++ {
		id, err := store.InsertAllocation(ctx, pool, f.eventID, fmt.Sprintf("c%d", i), store.KindConfirmed, time.Duration(120-i)*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		f.confirmed = append(f.confirmed, id)
	}
	for i := 1; i <= waitlist; i++ {
		if _, err := store.InsertAllocation(ctx, pool, f.eventID, fmt.Sprintf("w%d", i), store.KindWaitlist, time.Duration(60-i)*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) cancelFirst(t *testing.T, key bool) {
	t.Helper()
	alloc := f.confirmed[0]
	if err := store.CancelConfirmed(f.ctx, f.pool, alloc, -time.Minute); err != nil {
		t.Fatal(err)
	}
	k := ""
	if key {
		k = store.ConversionKey(alloc)
	}
	if _, err := store.EnqueueConversion(f.ctx, f.pool, f.eventID, alloc, k); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) step(t *testing.T, s worker.Stepper) bool {
	t.Helper()
	did, err := s.Step(f.ctx)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return did
}

func (f *fixture) forceRedelivery(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE conversion_jobs SET status = 'pending', claimed_at = NULL, claimed_by = NULL WHERE event_id = $1`, f.eventID); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) active(t *testing.T) int {
	t.Helper()
	n, err := store.CountActiveConfirmed(f.ctx, f.pool, f.eventID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) waiting(t *testing.T) int {
	t.Helper()
	w, err := store.ActiveWaitlist(f.ctx, f.pool, f.eventID, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(w)
}

func (f *fixture) verdict(t *testing.T) oracle.Verdict {
	t.Helper()
	v, err := oracle.Check(f.ctx, f.pool, f.eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *fixture) opts(t *testing.T, v worker.Variant) worker.Options {
	t.Helper()
	inj, err := fault.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	return worker.Options{
		Variant: v, EventID: f.eventID, WorkerID: t.Name(),
		Lease: time.Hour, Poll: 100 * time.Millisecond,
		Fault: inj, Log: log.New(io.Discard, "", 0),
	}
}

// The reset stands in for a crash between commit and ack, which the s1 scenario tests for real.
func TestNaiveRedeliveryOverflows(t *testing.T) {
	f := setup(t, 2, 2, 3, time.Hour)
	f.cancelFirst(t, false)
	s := worker.NewConversion(f.pool, f.opts(t, worker.Naive))

	f.step(t, s)
	f.forceRedelivery(t)
	f.step(t, s)

	if n := f.active(t); n != 3 {
		t.Fatalf("active confirmed = %d, want 3 (capacity 2)", n)
	}
	if v := f.verdict(t); !slices.Contains(v.Violations(), oracle.Capacity) {
		t.Fatalf("oracle violations = %v, want %s", v.Violations(), oracle.Capacity)
	}
}

func TestSafeAcksInSameTransaction(t *testing.T) {
	f := setup(t, 2, 2, 3, time.Hour)
	f.cancelFirst(t, true)
	s := worker.NewConversion(f.pool, f.opts(t, worker.Safe))

	if !f.step(t, s) {
		t.Fatal("first step did no work")
	}
	counts, err := store.JobCounts(f.ctx, f.pool, f.eventID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["done"] != 1 || len(counts) != 1 {
		t.Fatalf("job counts = %v, want one done job", counts)
	}
	if f.step(t, s) {
		t.Fatal("second step claimed a job that was already done")
	}
	if n := f.active(t); n != 2 {
		t.Fatalf("active confirmed = %d, want 2", n)
	}
}

func TestSafeRecountsOnRedelivery(t *testing.T) {
	f := setup(t, 2, 2, 3, time.Hour)
	f.cancelFirst(t, true)
	s := worker.NewConversion(f.pool, f.opts(t, worker.Safe))

	f.step(t, s)
	f.forceRedelivery(t)
	f.step(t, s)

	if n := f.active(t); n != 2 {
		t.Fatalf("active confirmed = %d, want 2", n)
	}
	if v := f.verdict(t); v.Overall != oracle.OverallPass {
		t.Fatalf("oracle = %s %v, want PASS", v.Overall, v.Violations())
	}
}

func TestNaiveConvertsAfterDeadline(t *testing.T) {
	f := setup(t, 2, 2, 3, -time.Second)
	f.cancelFirst(t, false)
	if err := store.MarkDeadlineProcessed(f.ctx, f.pool, f.eventID); err != nil {
		t.Fatal(err)
	}
	before := f.waiting(t)
	f.step(t, worker.NewConversion(f.pool, f.opts(t, worker.Naive)))

	if after := f.waiting(t); after != before-1 {
		t.Fatalf("active waitlist entries: before=%d after=%d, want one conversion after the deadline", before, after)
	}
}

func TestSafeNoOpAfterDeadline(t *testing.T) {
	f := setup(t, 2, 2, 3, -time.Second)
	f.cancelFirst(t, true)
	if err := store.MarkDeadlineProcessed(f.ctx, f.pool, f.eventID); err != nil {
		t.Fatal(err)
	}
	before, waiting := f.active(t), f.waiting(t)
	f.step(t, worker.NewConversion(f.pool, f.opts(t, worker.Safe)))

	if f.active(t) != before || f.waiting(t) != waiting {
		t.Fatal("safe worker changed state after the deadline was finalised")
	}
}

func TestSafeDeadlineFinalises(t *testing.T) {
	f := setup(t, 5, 3, 5, -time.Second)
	f.step(t, worker.NewDeadline(f.pool, f.opts(t, worker.Safe)))

	if n := f.active(t); n != 5 {
		t.Fatalf("active confirmed = %d, want 5", n)
	}
	if n := f.waiting(t); n != 0 {
		t.Fatalf("active waitlist entries = %d, want 0", n)
	}
	if v := f.verdict(t); v.Overall != oracle.OverallPass {
		t.Fatalf("oracle = %s %v, want PASS", v.Overall, v.Violations())
	}
}
