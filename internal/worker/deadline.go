package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/domain"
	"waitlist-chaos/internal/fault"
	"waitlist-chaos/internal/store"
)

type deadline struct {
	pool *pgxpool.Pool
	opts Options
}

func NewDeadline(pool *pgxpool.Pool, opts Options) Stepper {
	return &deadline{pool: pool, opts: opts}
}

func (d *deadline) Idle(ctx context.Context) (bool, error) {
	due, err := store.AnyDueEvent(ctx, d.pool, d.opts.EventID)
	return !due, err
}

func (d *deadline) Step(ctx context.Context) (bool, error) {
	if d.opts.Variant == Naive {
		return d.stepNaive(ctx)
	}
	return d.stepSafe(ctx)
}

// stepNaive has deliberate defects: marks processed before writing, uses no row locks, and has no status guards.
func (d *deadline) stepNaive(ctx context.Context) (bool, error) {
	id, ok, err := store.DueEvent(ctx, d.pool, d.opts.EventID, false)
	if err != nil || !ok {
		return false, err
	}
	if err := store.MarkDeadlineProcessed(ctx, d.pool, id); err != nil {
		return true, err
	}
	d.opts.Log.Printf("event %s: deadline reached, marked processed", short(id))
	if err := d.opts.Fault.At(ctx, fault.DeadlineAfterMarkProcessed); err != nil {
		return true, err
	}

	ev, err := store.GetEvent(ctx, d.pool, id, false)
	if err != nil {
		return true, err
	}
	active, err := store.CountActiveConfirmed(ctx, d.pool, id)
	if err != nil {
		return true, err
	}
	waitlist, err := store.ActiveWaitlist(ctx, d.pool, id, false, 0)
	if err != nil {
		return true, err
	}
	convert, refund := domain.PlanDeadline(ev.Capacity-active, waitlist)
	d.logPlan(id, ev.Capacity-active, convert, refund)

	for _, e := range convert {
		if _, _, err := store.Convert(ctx, d.pool, e.ID, false); err != nil {
			return true, err
		}
	}
	if err := d.opts.Fault.At(ctx, fault.DeadlineAfterConversions); err != nil {
		return true, err
	}
	for _, e := range refund {
		if _, err := store.Refund(ctx, d.pool, e.ID, false, ""); err != nil {
			return true, err
		}
	}
	d.opts.Log.Printf("event %s: finalised (%d converted, %d refunded)", short(id), len(convert), len(refund))
	return true, nil
}

// stepSafe finalises the event in one transaction with row lock, status guards, and deterministic idempotency keys.
func (d *deadline) stepSafe(ctx context.Context) (bool, error) {
	var (
		id              string
		did             bool
		convert, refund []domain.Entry
	)
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var ok bool
		var err error
		id, ok, err = store.DueEvent(ctx, tx, d.opts.EventID, true)
		if err != nil || !ok {
			return err
		}
		did = true
		ev, err := store.GetEvent(ctx, tx, id, true)
		if err != nil {
			return err
		}
		active, err := store.CountActiveConfirmed(ctx, tx, id)
		if err != nil {
			return err
		}
		waitlist, err := store.ActiveWaitlist(ctx, tx, id, true, 0)
		if err != nil {
			return err
		}
		convert, refund = domain.PlanDeadline(ev.Capacity-active, waitlist)
		d.logPlan(id, ev.Capacity-active, convert, refund)

		for _, e := range convert {
			if _, ok, err := store.Convert(ctx, tx, e.ID, true); err != nil || !ok {
				return orChanged(err, e)
			}
		}
		if err := d.opts.Fault.At(ctx, fault.DeadlineAfterConversions); err != nil {
			return err
		}
		for _, e := range refund {
			if ok, err := store.Refund(ctx, tx, e.ID, true, store.RefundKey(e.ID)); err != nil || !ok {
				return orChanged(err, e)
			}
		}
		if err := store.MarkDeadlineProcessed(ctx, tx, id); err != nil {
			return err
		}
		return d.opts.Fault.At(ctx, fault.DeadlineAfterMarkProcessed)
	})
	if err != nil {
		return did, err
	}
	if did {
		d.opts.Log.Printf("event %s: finalised (%d converted, %d refunded)", short(id), len(convert), len(refund))
	}
	return did, nil
}

func orChanged(err error, e domain.Entry) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("waitlist entry %s (%s) changed while locked", e.ID, e.UserID)
}

func (d *deadline) logPlan(eventID string, free int, convert, refund []domain.Entry) {
	d.opts.Log.Printf("event %s: plan free_seats=%d convert=[%s] refund=[%s]",
		short(eventID), free, users(convert), users(refund))
}

func users(es []domain.Entry) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.UserID
	}
	return strings.Join(out, " ")
}
