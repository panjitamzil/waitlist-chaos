package worker

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/fault"
)

type Variant string

const (
	Naive Variant = "naive"
	Safe  Variant = "safe"
)

func ParseVariant(s string) (Variant, error) {
	switch v := Variant(s); v {
	case Naive, Safe:
		return v, nil
	}
	return "", fmt.Errorf("variant must be %q or %q, got %q", Naive, Safe, s)
}

type Options struct {
	Variant   Variant
	EventID   string
	WorkerID  string
	Lease     time.Duration
	Poll      time.Duration
	UntilIdle bool
	Fault     *fault.Injector
	Log       *log.Logger
}

// Stepper does at most one unit of work per call.
type Stepper interface {
	Step(ctx context.Context) (did bool, err error)
	Idle(ctx context.Context) (bool, error)
}

// Run polls Step until context ends or, if UntilIdle, until no work remains.
func Run(ctx context.Context, s Stepper, opts Options) error {
	for {
		did, err := s.Step(ctx)
		if err != nil {
			return err
		}
		if did {
			continue
		}
		if opts.UntilIdle {
			idle, err := s.Idle(ctx)
			if err != nil {
				return err
			}
			if idle {
				opts.Log.Printf("no outstanding work, exiting")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Poll):
		}
	}
}

// Main is the shared entry point for worker binaries.
func Main(name string, newStepper func(*pgxpool.Pool, Options) Stepper) {
	variant := flag.String("variant", string(Safe), "implementation variant: naive or safe")
	eventID := flag.String("event", "", "only process this event id (default: all events)")
	untilIdle := flag.Bool("until-idle", false, "exit once no outstanding work remains")
	lease := flag.Duration("lease", 2*time.Second, "how long a claimed job stays reserved before another worker may reclaim it")
	poll := flag.Duration("poll", 200*time.Millisecond, "polling interval")
	workerID := flag.String("id", "", "worker instance id (default: <name>-<pid>)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.Ltime|log.Lmicroseconds)
	fail := func(err error) {
		logger.Printf("error: %v", err)
		os.Exit(1)
	}

	inj, err := fault.FromEnv()
	if err != nil {
		fail(err)
	}
	v, err := ParseVariant(*variant)
	if err != nil {
		fail(err)
	}
	if *workerID == "" {
		*workerID = fmt.Sprintf("%s-%d", name, os.Getpid())
	}
	opts := Options{
		Variant: v, EventID: *eventID, WorkerID: *workerID,
		Lease: *lease, Poll: *poll, UntilIdle: *untilIdle,
		Fault: inj, Log: logger,
	}
	if err := run(name, newStepper, opts); err != nil {
		fail(err)
	}
}

func run(name string, newStepper func(*pgxpool.Pool, Options) Stepper, opts Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	opts.Log.Printf("%s started: id=%s variant=%s event=%s fault=%s", name, opts.WorkerID, opts.Variant, orAll(opts.EventID), opts.Fault)
	err = Run(ctx, newStepper(pool, opts), opts)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func orAll(eventID string) string {
	if eventID == "" {
		return "all"
	}
	return short(eventID)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
