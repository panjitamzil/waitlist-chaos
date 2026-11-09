//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/store"
)

func testPool(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	return ctx, pool
}

func seedEvent(t *testing.T, ctx context.Context, q store.DBTX, capacity int) string {
	t.Helper()
	id, err := store.CreateEvent(ctx, q, "test", "store-test", "safe", capacity, time.Hour)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	return id
}

func seedWaitlist(t *testing.T, ctx context.Context, q store.DBTX, eventID string, users []string) []string {
	t.Helper()
	var ids []string
	for i, u := range users {
		id, err := store.InsertAllocation(ctx, q, eventID, u, store.KindWaitlist, time.Duration(len(users)-i)*time.Minute)
		if err != nil {
			t.Fatalf("InsertAllocation %s: %v", u, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func cancelledSeat(t *testing.T, ctx context.Context, q store.DBTX, eventID string) string {
	t.Helper()
	id, err := store.InsertAllocation(ctx, q, eventID, "c1", store.KindConfirmed, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelConfirmed(ctx, q, id, 0); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConvertGuardedSkipsInactiveEntry(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	w := seedWaitlist(t, ctx, pool, eventID, []string{"w1", "w2"})

	if id, ok, err := store.Convert(ctx, pool, w[0], true); err != nil || !ok || id == "" {
		t.Fatalf("first convert: ok=%v id=%q err=%v", ok, id, err)
	}
	if id, ok, err := store.Convert(ctx, pool, w[0], true); err != nil || ok || id != "" {
		t.Fatalf("second guarded convert should be a no-op: ok=%v id=%q err=%v", ok, id, err)
	}
	if id, ok, err := store.Convert(ctx, pool, w[1], true); err != nil || !ok || id == "" {
		t.Fatalf("convert w2: ok=%v id=%q err=%v", ok, id, err)
	}
}

func TestRefundGuardedSkipsConvertedEntry(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	w := seedWaitlist(t, ctx, pool, eventID, []string{"w1"})

	if _, _, err := store.Convert(ctx, pool, w[0], true); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Refund(ctx, pool, w[0], true, store.RefundKey(w[0])); err != nil || ok {
		t.Fatalf("guarded refund of a converted entry: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := store.Refund(ctx, pool, w[0], false, ""); err != nil || !ok {
		t.Fatalf("unguarded refund: ok=%v err=%v, want ok=true", ok, err)
	}
}

func TestRefundKeyWritesOneLedgerRow(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	w := seedWaitlist(t, ctx, pool, eventID, []string{"w1"})

	key := store.RefundKey(w[0])
	for i := 0; i < 2; i++ {
		if _, err := store.Refund(ctx, pool, w[0], false, key); err != nil {
			t.Fatalf("refund %d: %v", i+1, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refund_ledger WHERE allocation_id = $1`, w[0]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}

func TestRandomRefundKeysWriteTwoLedgerRows(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	w := seedWaitlist(t, ctx, pool, eventID, []string{"w1"})

	for i := 0; i < 2; i++ {
		if _, err := store.Refund(ctx, pool, w[0], false, ""); err != nil {
			t.Fatalf("refund %d: %v", i+1, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refund_ledger WHERE allocation_id = $1`, w[0]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ledger rows = %d, want 2", n)
	}
}

func TestEnqueueConversionDedup(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	cID := cancelledSeat(t, ctx, pool, eventID)

	key := store.ConversionKey(cID)
	if ok, err := store.EnqueueConversion(ctx, pool, eventID, cID, key); err != nil || !ok {
		t.Fatalf("first enqueue: ok=%v err=%v", ok, err)
	}
	if ok, err := store.EnqueueConversion(ctx, pool, eventID, cID, key); err != nil || ok {
		t.Fatalf("duplicate enqueue: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := store.EnqueueConversion(ctx, pool, eventID, cID, ""); err != nil || !ok {
		t.Fatalf("enqueue without key: ok=%v err=%v, want a new job", ok, err)
	}
}

func TestActiveWaitlistFIFO(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	seedWaitlist(t, ctx, pool, eventID, []string{"w3", "w1", "w2"})

	entries, err := store.ActiveWaitlist(ctx, pool, eventID, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"w3", "w1", "w2"}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		if e.UserID != want[i] {
			t.Errorf("position %d: got %s, want %s", i, e.UserID, want[i])
		}
	}
}

func TestClaimJobAfterLeaseExpiry(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	cID := cancelledSeat(t, ctx, pool, eventID)
	if _, err := store.EnqueueConversion(ctx, pool, eventID, cID, ""); err != nil {
		t.Fatal(err)
	}

	j1, err := store.ClaimConversionJob(ctx, pool, eventID, "worker-a", time.Millisecond)
	if err != nil || j1 == nil {
		t.Fatalf("first claim: job=%v err=%v", j1, err)
	}
	time.Sleep(50 * time.Millisecond)
	j2, err := store.ClaimConversionJob(ctx, pool, eventID, "worker-b", time.Millisecond)
	if err != nil || j2 == nil {
		t.Fatalf("reclaim after lease expiry: job=%v err=%v", j2, err)
	}
	if j2.ID != j1.ID || j2.Attempts != 2 {
		t.Fatalf("reclaimed job %s attempt %d, want %s attempt 2", j2.ID, j2.Attempts, j1.ID)
	}
}

func TestClaimSkipsLeasedJob(t *testing.T) {
	ctx, pool := testPool(t)
	eventID := seedEvent(t, ctx, pool, 5)
	cID := cancelledSeat(t, ctx, pool, eventID)
	if _, err := store.EnqueueConversion(ctx, pool, eventID, cID, ""); err != nil {
		t.Fatal(err)
	}

	if j, err := store.ClaimConversionJob(ctx, pool, eventID, "worker-a", time.Hour); err != nil || j == nil {
		t.Fatalf("first claim: job=%v err=%v", j, err)
	}
	if j, err := store.ClaimConversionJob(ctx, pool, eventID, "worker-b", time.Hour); err != nil || j != nil {
		t.Fatalf("second claim inside the lease: job=%v err=%v, want none", j, err)
	}
}
