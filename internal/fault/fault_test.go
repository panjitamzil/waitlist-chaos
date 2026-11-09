package fault

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNoFaultConfigured(t *testing.T) {
	inj, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := inj.At(context.Background(), AfterConversionCommit); err != nil {
		t.Fatal(err)
	}
	if inj.String() != "none" {
		t.Fatalf("String() = %q", inj.String())
	}
}

func TestInvalidAction(t *testing.T) {
	if _, err := New(AfterConversionCommit, "explode"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCrashFiresOnlyAtMatchingPointAndOnce(t *testing.T) {
	inj, err := New(AfterConversionCommit, ActionCrash)
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	var codes []int
	inj.out = &out
	inj.exit = func(c int) { codes = append(codes, c) }

	_ = inj.At(context.Background(), DeadlineAfterConversions)
	if len(codes) != 0 {
		t.Fatal("fault fired at the wrong point")
	}
	_ = inj.At(context.Background(), AfterConversionCommit)
	_ = inj.At(context.Background(), AfterConversionCommit)
	if len(codes) != 1 || codes[0] != CrashExitCode {
		t.Fatalf("exit codes = %v, want [%d]", codes, CrashExitCode)
	}
	if !strings.Contains(out.String(), CrashMarker+" "+AfterConversionCommit) {
		t.Fatalf("missing crash marker in %q", out.String())
	}
}

func TestPauseResumesOnSIGUSR1(t *testing.T) {
	inj, err := New(DeadlineAfterConversions, ActionPause)
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	inj.out = &out

	done := make(chan error, 1)
	go func() { done <- inj.At(context.Background(), DeadlineAfterConversions) }()

	waitFor(t, func() bool { return strings.Contains(out.String(), PausedMarker) })
	select {
	case <-done:
		t.Fatal("At returned before resume")
	case <-time.After(50 * time.Millisecond):
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("At did not resume")
	}
	if !strings.Contains(out.String(), ResumedMarker) {
		t.Fatalf("missing resume marker in %q", out.String())
	}
}

func TestPauseHonoursContextCancel(t *testing.T) {
	inj, err := New(DeadlineAfterConversions, ActionPause)
	if err != nil {
		t.Fatal(err)
	}
	inj.out = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inj.At(ctx, DeadlineAfterConversions); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
