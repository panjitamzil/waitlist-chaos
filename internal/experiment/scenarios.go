package experiment

import (
	"context"
	"time"

	"waitlist-chaos/internal/fault"
	"waitlist-chaos/internal/oracle"
	"waitlist-chaos/internal/worker"
)

type Scenario struct {
	ID     string
	Title  string
	Expect map[worker.Variant]Expectation
	run    func(ctx context.Context, e *env) error
}

var pass = Expectation{Overall: oracle.OverallPass}

func expect(naive Expectation) map[worker.Variant]Expectation {
	return map[worker.Variant]Expectation{worker.Naive: naive, worker.Safe: pass}
}

func Scenarios() []Scenario {
	return []Scenario{
		{
			ID:     "s0",
			Title:  "baseline: deadline finalisation without faults",
			Expect: expect(pass),
			run: func(ctx context.Context, e *env) error {
				if err := e.seed(ctx, seedSpec{capacity: 5, confirmed: 3, waitlist: 5, deadlineIn: -time.Second}); err != nil {
					return err
				}
				d, err := e.start(DeadlineBin, "", "")
				if err != nil {
					return err
				}
				return e.finish(ctx, d, DeadlineBin)
			},
		},
		{
			ID:     "s1",
			Title:  "conversion worker crashes after committing, before acknowledging the job",
			Expect: expect(Expectation{Overall: oracle.OverallSafety, Violations: []oracle.CheckID{oracle.Capacity}}),
			run: func(ctx context.Context, e *env) error {
				if err := e.seed(ctx, seedSpec{capacity: 5, confirmed: 5, waitlist: 5, deadlineIn: time.Hour}); err != nil {
					return err
				}
				if err := e.cancel(ctx, 1, 0, 1); err != nil {
					return err
				}
				c, err := e.start(ConversionBin, fault.AfterConversionCommit, fault.ActionCrash)
				if err != nil {
					return err
				}
				if err := e.expectCrash(ctx, c); err != nil {
					return err
				}
				c, err = e.start(ConversionBin, "", "")
				if err != nil {
					return err
				}
				return e.finish(ctx, c, ConversionBin)
			},
		},
		{
			ID:     "s2",
			Title:  "the same cancellation is delivered twice",
			Expect: expect(Expectation{Overall: oracle.OverallSafety, Violations: []oracle.CheckID{oracle.Capacity}}),
			run: func(ctx context.Context, e *env) error {
				if err := e.seed(ctx, seedSpec{capacity: 5, confirmed: 5, waitlist: 5, deadlineIn: time.Hour}); err != nil {
					return err
				}
				if err := e.cancel(ctx, 1, 0, 2); err != nil {
					return err
				}
				c, err := e.start(ConversionBin, "", "")
				if err != nil {
					return err
				}
				return e.finish(ctx, c, ConversionBin)
			},
		},
		{
			ID:     "s3",
			Title:  "a lagging conversion job races the deadline worker",
			Expect: expect(Expectation{Overall: oracle.OverallSafety, Violations: []oracle.CheckID{oracle.Capacity, oracle.ConvertedXorRefunded}}),
			run: func(ctx context.Context, e *env) error {
				if err := e.seed(ctx, seedSpec{capacity: 5, confirmed: 5, waitlist: 5, deadlineIn: -time.Second}); err != nil {
					return err
				}
				if err := e.cancel(ctx, 1, -time.Minute, 1); err != nil {
					return err
				}
				d, err := e.start(DeadlineBin, fault.DeadlineAfterConversions, fault.ActionPause)
				if err != nil {
					return err
				}
				if _, err := d.waitPaused(ctx); err != nil {
					return err
				}
				e.logf("deadline worker paused after conversions, before refunds; starting conversion worker")
				c, err := e.start(ConversionBin, "", "")
				if err != nil {
					return err
				}
				if !c.exitsWithin(3 * time.Second) {
					e.logf("conversion worker still running (blocked, e.g. on the event row lock)")
				}
				e.logf("resuming deadline worker")
				if err := d.resume(); err != nil {
					return err
				}
				if err := e.finish(ctx, d, DeadlineBin); err != nil {
					return err
				}
				return e.finish(ctx, c, ConversionBin)
			},
		},
		{
			ID:     "s4",
			Title:  "deadline worker crashes mid-finalisation",
			Expect: expect(Expectation{Overall: oracle.OverallLiveness, Violations: []oracle.CheckID{oracle.Resolution}}),
			run: func(ctx context.Context, e *env) error {
				if err := e.seed(ctx, seedSpec{capacity: 5, confirmed: 3, waitlist: 5, deadlineIn: -time.Second}); err != nil {
					return err
				}
				d, err := e.start(DeadlineBin, fault.DeadlineAfterMarkProcessed, fault.ActionCrash)
				if err != nil {
					return err
				}
				if err := e.expectCrash(ctx, d); err != nil {
					return err
				}
				d, err = e.start(DeadlineBin, "", "")
				if err != nil {
					return err
				}
				return e.finish(ctx, d, DeadlineBin)
			},
		},
	}
}

// Select returns scenarios matching id, or all scenarios if id is "all".
func Select(id string) []Scenario {
	all := Scenarios()
	if id == "all" {
		return all
	}
	for _, s := range all {
		if s.ID == id {
			return []Scenario{s}
		}
	}
	return nil
}
