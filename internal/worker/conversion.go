package worker

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/fault"
	"waitlist-chaos/internal/store"
)

type conversion struct {
	pool *pgxpool.Pool
	opts Options
}

func NewConversion(pool *pgxpool.Pool, opts Options) Stepper {
	return &conversion{pool: pool, opts: opts}
}

func (c *conversion) Idle(ctx context.Context) (bool, error) {
	busy, err := store.OutstandingJobs(ctx, c.pool, c.opts.EventID)
	return !busy, err
}

func (c *conversion) Step(ctx context.Context) (bool, error) {
	job, err := store.ClaimConversionJob(ctx, c.pool, c.opts.EventID, c.opts.WorkerID, c.opts.Lease)
	if err != nil || job == nil {
		return false, err
	}
	c.opts.Log.Printf("claimed job %s (attempt %d)", short(job.ID), job.Attempts)
	if c.opts.Variant == Naive {
		return true, c.processNaive(ctx, job)
	}
	return true, c.processSafe(ctx, job)
}

// processNaive has deliberate defects: trusts capacity without recounting, ignores deadline state, and uses separate transactions.
func (c *conversion) processNaive(ctx context.Context, job *store.Job) error {
	var confirmedID, user string
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		head, err := store.ActiveWaitlist(ctx, tx, job.EventID, false, 1)
		if err != nil || len(head) == 0 {
			return err
		}
		user = head[0].UserID
		confirmedID, _, err = store.Convert(ctx, tx, head[0].ID, false)
		return err
	})
	if err != nil {
		return err
	}
	if confirmedID == "" {
		c.opts.Log.Printf("job %s: waitlist empty, nothing to convert", short(job.ID))
	} else {
		c.opts.Log.Printf("job %s: converted %s to a confirmed seat", short(job.ID), user)
	}
	if err := c.opts.Fault.At(ctx, fault.AfterConversionCommit); err != nil {
		return err
	}
	return store.MarkJobDone(ctx, c.pool, job.ID, confirmedID)
}

// processSafe locks the job and event, recounts free seats, and completes everything atomically.
func (c *conversion) processSafe(ctx context.Context, job *store.Job) error {
	var outcome string
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		status, err := store.JobStatus(ctx, tx, job.ID, true)
		if err != nil {
			return err
		}
		if status == "done" {
			outcome = "already completed by another attempt"
			return nil
		}
		ev, err := store.GetEvent(ctx, tx, job.EventID, true)
		if err != nil {
			return err
		}
		cancelledAt, err := store.CancelledAt(ctx, tx, job.CancelledAllocID)
		if err != nil {
			return err
		}
		done := func(reason string) error {
			outcome = reason
			return store.MarkJobDone(ctx, tx, job.ID, "")
		}
		switch {
		case ev.Processed:
			return done("no-op: deadline already finalised, its outcome is final")
		case cancelledAt == nil || !cancelledAt.Before(ev.DeadlineAt):
			return done("no-op: cancellation is not before the deadline")
		}
		active, err := store.CountActiveConfirmed(ctx, tx, job.EventID)
		if err != nil {
			return err
		}
		if active >= ev.Capacity {
			return done(fmt.Sprintf("no-op: no free seat (%d/%d confirmed)", active, ev.Capacity))
		}
		head, err := store.ActiveWaitlist(ctx, tx, job.EventID, true, 1)
		if err != nil {
			return err
		}
		if len(head) == 0 {
			return done("no-op: waitlist empty")
		}
		confirmedID, ok, err := store.Convert(ctx, tx, head[0].ID, true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("waitlist entry %s changed while locked", head[0].ID)
		}
		outcome = fmt.Sprintf("converted %s to a confirmed seat", head[0].UserID)
		return store.MarkJobDone(ctx, tx, job.ID, confirmedID)
	})
	if err != nil {
		return err
	}
	c.opts.Log.Printf("job %s: %s", short(job.ID), outcome)
	return c.opts.Fault.At(ctx, fault.AfterConversionCommit)
}
