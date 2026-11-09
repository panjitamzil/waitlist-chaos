package fault

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const (
	PausedMarker  = "FAULT_PAUSED"
	ResumedMarker = "FAULT_RESUMED"
	CrashMarker   = "FAULT_CRASH"
	CrashExitCode = 137 // matches SIGKILL exit status

	AfterConversionCommit      = "after_conversion_commit"
	DeadlineAfterConversions   = "deadline_after_conversions"
	DeadlineAfterMarkProcessed = "deadline_after_mark_processed"
)

type Action string

const (
	ActionCrash Action = "crash"
	ActionPause Action = "pause"
)

type Injector struct {
	point  string
	action Action
	out    io.Writer
	exit   func(int)
	resume chan os.Signal

	mu    sync.Mutex
	fired bool
}

// FromEnv reads FAULT_POINT and FAULT_ACTION from the environment.
func FromEnv() (*Injector, error) {
	return New(os.Getenv("FAULT_POINT"), Action(os.Getenv("FAULT_ACTION")))
}

func New(point string, action Action) (*Injector, error) {
	inj := &Injector{point: point, action: action, out: os.Stderr, exit: os.Exit}
	if point == "" {
		return inj, nil
	}
	switch action {
	case ActionCrash:
	case ActionPause:
		inj.resume = make(chan os.Signal, 1)
		signal.Notify(inj.resume, syscall.SIGUSR1)
	default:
		return nil, fmt.Errorf("FAULT_ACTION must be %q or %q, got %q", ActionCrash, ActionPause, action)
	}
	return inj, nil
}

func (i *Injector) String() string {
	if i == nil || i.point == "" {
		return "none"
	}
	return fmt.Sprintf("%s@%s", i.action, i.point)
}

// At fires the configured fault at this point; each fault fires at most once.
func (i *Injector) At(ctx context.Context, point string) error {
	if i == nil || i.point == "" || i.point != point {
		return nil
	}
	i.mu.Lock()
	if i.fired {
		i.mu.Unlock()
		return nil
	}
	i.fired = true
	i.mu.Unlock()

	switch i.action {
	case ActionCrash:
		fmt.Fprintf(i.out, "%s %s\n", CrashMarker, point)
		i.exit(CrashExitCode)
	case ActionPause:
		fmt.Fprintf(i.out, "%s %s\n", PausedMarker, point)
		select {
		case <-i.resume:
			fmt.Fprintf(i.out, "%s %s\n", ResumedMarker, point)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
