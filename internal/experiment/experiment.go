package experiment

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/fault"
	"waitlist-chaos/internal/health"
	"waitlist-chaos/internal/oracle"
	"waitlist-chaos/internal/store"
	"waitlist-chaos/internal/worker"
)

const (
	ConversionBin = "conversion-worker"
	DeadlineBin   = "deadline-worker"
)

type Config struct {
	BinDir  string
	Lease   time.Duration
	Timeout time.Duration
	Log     func(format string, args ...any)
}

type Expectation struct {
	Overall    oracle.Overall
	Violations []oracle.CheckID
}

func (e Expectation) String() string {
	if len(e.Violations) == 0 {
		return string(e.Overall)
	}
	return fmt.Sprintf("%s %s", e.Overall, joinIDs(e.Violations))
}

type Result struct {
	Scenario string
	Variant  worker.Variant
	EventID  string
	Health   health.Report
	Verdict  oracle.Verdict
	Expect   Expectation
	Match    bool
	Duration time.Duration
	Err      error
}

type env struct {
	cfg       Config
	pool      *pgxpool.Pool
	scenario  string
	variant   worker.Variant
	eventID   string
	confirmed []string
	waitlist  []string
	finalExit map[string]int
	crashes   int
	procs     []*proc
	seq       int
}

// Run executes one scenario/variant and returns the combined result.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config, sc Scenario, v worker.Variant) Result {
	start := time.Now()
	res := Result{Scenario: sc.ID, Variant: v, Expect: sc.Expect[v]}
	e := &env{cfg: cfg, pool: pool, scenario: sc.ID, variant: v, finalExit: map[string]int{}}

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	err := sc.run(runCtx, e)
	cancel()
	for _, p := range e.procs {
		p.kill()
	}
	res.EventID = e.eventID
	if err != nil {
		res.Err, res.Duration = err, time.Since(start)
		return res
	}

	if res.Health, err = health.Check(ctx, pool, e.eventID, e.finalExit, e.crashes); err != nil {
		res.Err = err
	} else if res.Verdict, err = oracle.Check(ctx, pool, e.eventID, 0); err != nil {
		res.Err = err
	}
	res.Match = res.Err == nil &&
		res.Health.Status == health.Green &&
		res.Verdict.Overall == res.Expect.Overall &&
		slices.Equal(res.Verdict.Violations(), res.Expect.Violations)
	res.Duration = time.Since(start)
	return res
}

func (e *env) logf(format string, args ...any) { e.cfg.Log(format, args...) }

type seedSpec struct {
	capacity, confirmed, waitlist int
	deadlineIn                    time.Duration
}

// seed creates an event with cN confirmed holders and wM waitlist entries in purchase order.
func (e *env) seed(ctx context.Context, s seedSpec) error {
	id, err := store.CreateEvent(ctx, e.pool, e.scenario+"/"+string(e.variant), e.scenario, string(e.variant), s.capacity, s.deadlineIn)
	if err != nil {
		return err
	}
	e.eventID = id
	for i := 1; i <= s.confirmed; i++ {
		a, err := store.InsertAllocation(ctx, e.pool, id, fmt.Sprintf("c%d", i), store.KindConfirmed, time.Duration(120-i)*time.Minute)
		if err != nil {
			return err
		}
		e.confirmed = append(e.confirmed, a)
	}
	for i := 1; i <= s.waitlist; i++ {
		a, err := store.InsertAllocation(ctx, e.pool, id, fmt.Sprintf("w%d", i), store.KindWaitlist, time.Duration(60-i)*time.Minute)
		if err != nil {
			return err
		}
		e.waitlist = append(e.waitlist, a)
	}
	e.logf("seeded event %s: capacity=%d confirmed=%d waitlist=%d deadline_in=%s",
		id[:8], s.capacity, s.confirmed, s.waitlist, s.deadlineIn)
	return nil
}

// cancel cancels confirmed seat n and enqueues a conversion job the given number of times.
func (e *env) cancel(ctx context.Context, n int, at time.Duration, deliveries int) error {
	alloc := e.confirmed[n-1]
	if err := store.CancelConfirmed(ctx, e.pool, alloc, at); err != nil {
		return err
	}
	key := ""
	if e.variant == worker.Safe {
		key = store.ConversionKey(alloc)
	}
	for i := 1; i <= deliveries; i++ {
		inserted, err := store.EnqueueConversion(ctx, e.pool, e.eventID, alloc, key)
		if err != nil {
			return err
		}
		e.logf("c%d cancelled; conversion job delivery %d/%d (new job: %v)", n, i, deliveries, inserted)
	}
	return nil
}

// start launches a worker binary with an optional fault point.
func (e *env) start(bin, point string, action fault.Action) (*proc, error) {
	e.seq++
	name := fmt.Sprintf("%s#%d", bin, e.seq)
	args := []string{
		"-variant", string(e.variant), "-event", e.eventID, "-until-idle",
		"-lease", e.cfg.Lease.String(), "-id", name,
	}
	var extra []string
	if point != "" {
		extra = []string{"FAULT_POINT=" + point, "FAULT_ACTION=" + string(action)}
	}
	p, err := startProc(name, filepath.Join(e.cfg.BinDir, bin), args, extra, e.cfg.Log)
	if err != nil {
		return nil, err
	}
	e.procs = append(e.procs, p)
	return p, nil
}

// expectCrash waits for a worker to crash with the injected exit code.
func (e *env) expectCrash(ctx context.Context, p *proc) error {
	code, err := p.wait(ctx)
	if err != nil {
		return err
	}
	if code != fault.CrashExitCode {
		return fmt.Errorf("%s exited with %d, expected injected crash (%d)", p.name, code, fault.CrashExitCode)
	}
	e.crashes++
	e.logf("%s crashed as injected; supervisor restarts it", p.name)
	return nil
}

// finish waits for a worker to exit and records its exit code.
func (e *env) finish(ctx context.Context, p *proc, bin string) error {
	code, err := p.wait(ctx)
	if err != nil {
		return err
	}
	e.finalExit[bin] = code
	return nil
}

// PrintReport writes the scenario result table and violation details.
func PrintReport(w io.Writer, results []Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tVARIANT\tHEALTH\tORACLE\tVIOLATED\tEXPECTED\tRESULT")
	for _, r := range results {
		healthCol, oracleCol, violated, outcome := "-", "-", "-", "MATCH"
		if r.Err == nil {
			healthCol, oracleCol = string(r.Health.Status), string(r.Verdict.Overall)
			if v := r.Verdict.Violations(); len(v) > 0 {
				violated = joinIDs(v)
			}
		}
		switch {
		case r.Err != nil:
			outcome = "ERROR"
		case !r.Match:
			outcome = "MISMATCH"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Scenario, r.Variant, healthCol, oracleCol, violated, r.Expect, outcome)
	}
	tw.Flush()

	for _, r := range results {
		label := fmt.Sprintf("%s/%s", r.Scenario, r.Variant)
		if r.Err != nil {
			fmt.Fprintf(w, "\n%s error: %v\n", label, r.Err)
			continue
		}
		var lines []string
		for _, reason := range r.Health.Reasons {
			lines = append(lines, "health: "+reason)
		}
		for _, res := range r.Verdict.Results {
			if res.Status != oracle.Violation {
				continue
			}
			for _, d := range res.Details {
				lines = append(lines, fmt.Sprintf("%s %s", res.ID, d))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(w, "\n%s (event %s, crashes recovered: %d)\n", label, r.EventID, r.Health.CrashesRecovered)
			for _, l := range lines {
				fmt.Fprintf(w, "  - %s\n", l)
			}
		}
	}
}

func joinIDs(ids []oracle.CheckID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, ",")
}
