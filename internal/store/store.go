package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"waitlist-chaos/internal/domain"
)

type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	KindConfirmed = "confirmed"
	KindWaitlist  = "waitlist"
)

type Event struct {
	ID         string
	Capacity   int
	DeadlineAt time.Time
	Processed  bool
}

type Job struct {
	ID               string
	EventID          string
	CancelledAllocID string
	Attempts         int
}

// ConversionKey returns the idempotency key for a conversion job triggered by a cancellation.
func ConversionKey(cancelledAllocID string) string { return "cancellation:" + cancelledAllocID }

// RefundKey returns the idempotency key for refunding an allocation.
func RefundKey(allocID string) string { return "refund:" + allocID }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func lockClause(lock bool) string {
	if lock {
		return " FOR UPDATE"
	}
	return ""
}

func CreateEvent(ctx context.Context, q DBTX, name, scenario, variant string, capacity int, deadlineIn time.Duration) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO events (name, scenario, variant, confirmed_capacity, deadline_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING id::text`,
		name, scenario, variant, capacity, deadlineIn.Seconds()).Scan(&id)
	return id, err
}

func InsertAllocation(ctx context.Context, q DBTX, eventID, userID, kind string, purchasedAgo time.Duration) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO allocations (event_id, user_id, kind, status, purchased_at)
		VALUES ($1, $2, $3, 'active', now() - make_interval(secs => $4))
		RETURNING id::text`,
		eventID, userID, kind, purchasedAgo.Seconds()).Scan(&id)
	return id, err
}

// CancelConfirmed cancels an active confirmed seat.
func CancelConfirmed(ctx context.Context, q DBTX, allocID string, at time.Duration) error {
	tag, err := q.Exec(ctx, `
		UPDATE allocations
		SET status = 'cancelled', cancelled_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 AND kind = 'confirmed' AND status = 'active'`,
		allocID, at.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("allocation %s is not an active confirmed seat", allocID)
	}
	return nil
}

// EnqueueConversion inserts a conversion job; returns false if the key already exists.
func EnqueueConversion(ctx context.Context, q DBTX, eventID, cancelledAllocID, key string) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO conversion_jobs (event_id, cancelled_alloc_id, status, idempotency_key)
		VALUES ($1, $2, 'pending', COALESCE(NULLIF($3, ''), gen_random_uuid()::text))
		ON CONFLICT (idempotency_key) DO NOTHING`,
		eventID, cancelledAllocID, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimConversionJob reserves the oldest pending job, or reclaims one whose lease expired.
func ClaimConversionJob(ctx context.Context, q DBTX, eventID, workerID string, lease time.Duration) (*Job, error) {
	var j Job
	err := q.QueryRow(ctx, `
		UPDATE conversion_jobs
		SET status = 'processing', claimed_by = $1, claimed_at = now(), attempts = attempts + 1
		WHERE id = (
			SELECT id FROM conversion_jobs
			WHERE (status = 'pending'
			       OR (status = 'processing' AND claimed_at < now() - make_interval(secs => $2)))
			  AND ($3::uuid IS NULL OR event_id = $3::uuid)
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id::text, event_id::text, cancelled_alloc_id::text, attempts`,
		workerID, lease.Seconds(), nullable(eventID)).Scan(&j.ID, &j.EventID, &j.CancelledAllocID, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// OutstandingJobs returns true if any job is pending or processing.
func OutstandingJobs(ctx context.Context, q DBTX, eventID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM conversion_jobs
			WHERE status IN ('pending', 'processing')
			  AND ($1::uuid IS NULL OR event_id = $1::uuid))`,
		nullable(eventID)).Scan(&ok)
	return ok, err
}

func JobStatus(ctx context.Context, q DBTX, jobID string, lock bool) (string, error) {
	var s string
	err := q.QueryRow(ctx, `SELECT status FROM conversion_jobs WHERE id = $1`+lockClause(lock), jobID).Scan(&s)
	return s, err
}

func MarkJobDone(ctx context.Context, q DBTX, jobID, convertedAllocID string) error {
	_, err := q.Exec(ctx, `
		UPDATE conversion_jobs
		SET status = 'done', completed_at = now(), converted_alloc_id = $2
		WHERE id = $1`,
		jobID, nullable(convertedAllocID))
	return err
}

// JobCounts returns a map of job status to count for an event.
func JobCounts(ctx context.Context, q DBTX, eventID string) (map[string]int, error) {
	rows, err := q.Query(ctx, `SELECT status, count(*) FROM conversion_jobs WHERE event_id = $1 GROUP BY status`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

func GetEvent(ctx context.Context, q DBTX, eventID string, lock bool) (Event, error) {
	var e Event
	err := q.QueryRow(ctx, `
		SELECT id::text, confirmed_capacity, deadline_at, deadline_processed
		FROM events WHERE id = $1`+lockClause(lock), eventID).Scan(&e.ID, &e.Capacity, &e.DeadlineAt, &e.Processed)
	return e, err
}

func CancelledAt(ctx context.Context, q DBTX, allocID string) (*time.Time, error) {
	var t *time.Time
	err := q.QueryRow(ctx, `SELECT cancelled_at FROM allocations WHERE id = $1`, allocID).Scan(&t)
	return t, err
}

func CountActiveConfirmed(ctx context.Context, q DBTX, eventID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM allocations
		WHERE event_id = $1 AND kind = 'confirmed' AND status = 'active'`, eventID).Scan(&n)
	return n, err
}

// ActiveWaitlist returns active waitlist entries in FIFO order; limit 0 means no limit.
func ActiveWaitlist(ctx context.Context, q DBTX, eventID string, lock bool, limit int) ([]domain.Entry, error) {
	sql := `
		SELECT id::text, user_id, purchased_at FROM allocations
		WHERE event_id = $1 AND kind = 'waitlist' AND status = 'active'
		ORDER BY purchased_at, id`
	if limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := q.Query(ctx, sql+lockClause(lock), eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Entry
	for rows.Next() {
		var e domain.Entry
		if err := rows.Scan(&e.ID, &e.UserID, &e.PurchasedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Convert turns a waitlist entry into a confirmed seat; guarded=true skips already-changed entries.
func Convert(ctx context.Context, q DBTX, waitlistID string, guarded bool) (confirmedID string, ok bool, err error) {
	sql := `UPDATE allocations SET status = 'converted', updated_at = now() WHERE id = $1`
	if guarded {
		sql += ` AND kind = 'waitlist' AND status = 'active'`
	}
	tag, err := q.Exec(ctx, sql, waitlistID)
	if err != nil {
		return "", false, err
	}
	if tag.RowsAffected() == 0 {
		return "", false, nil
	}
	err = q.QueryRow(ctx, `
		INSERT INTO allocations (event_id, user_id, kind, status, purchased_at, converted_from)
		SELECT event_id, user_id, 'confirmed', 'active', now(), id FROM allocations WHERE id = $1
		RETURNING id::text`, waitlistID).Scan(&confirmedID)
	if err != nil {
		return "", false, err
	}
	return confirmedID, true, nil
}

// Refund marks a waitlist entry refunded and writes a ledger record; guarded=true skips already-changed entries.
func Refund(ctx context.Context, q DBTX, allocID string, guarded bool, key string) (bool, error) {
	sql := `UPDATE allocations SET status = 'refunded', updated_at = now() WHERE id = $1`
	if guarded {
		sql += ` AND kind = 'waitlist' AND status = 'active'`
	}
	tag, err := q.Exec(ctx, sql, allocID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, err = q.Exec(ctx, `
		INSERT INTO refund_ledger (allocation_id, status, idempotency_key, completed_at)
		VALUES ($1, 'completed', COALESCE(NULLIF($2, ''), gen_random_uuid()::text), now())
		ON CONFLICT (idempotency_key) DO NOTHING`, allocID, key)
	return err == nil, err
}

// DueEvent returns one unprocessed event past its deadline; skipLocked=true requires a transaction.
func DueEvent(ctx context.Context, q DBTX, eventID string, skipLocked bool) (string, bool, error) {
	sql := `
		SELECT id::text FROM events
		WHERE deadline_at <= now() AND NOT deadline_processed
		  AND ($1::uuid IS NULL OR id = $1::uuid)
		ORDER BY deadline_at, id
		LIMIT 1`
	if skipLocked {
		sql += ` FOR UPDATE SKIP LOCKED`
	}
	var id string
	err := q.QueryRow(ctx, sql, nullable(eventID)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// AnyDueEvent reports whether any unprocessed event past its deadline exists.
func AnyDueEvent(ctx context.Context, q DBTX, eventID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM events
			WHERE deadline_at <= now() AND NOT deadline_processed
			  AND ($1::uuid IS NULL OR id = $1::uuid))`,
		nullable(eventID)).Scan(&ok)
	return ok, err
}

func MarkDeadlineProcessed(ctx context.Context, q DBTX, eventID string) error {
	_, err := q.Exec(ctx, `
		UPDATE events SET deadline_processed = true, deadline_processed_at = now()
		WHERE id = $1`, eventID)
	return err
}
