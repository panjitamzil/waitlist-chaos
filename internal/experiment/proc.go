package experiment

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"waitlist-chaos/internal/fault"
)

type proc struct {
	name   string
	cmd    *exec.Cmd
	paused chan string
	done   chan struct{}
	code   int
}

func startProc(name, path string, args, extraEnv []string, logf func(string, ...any)) (*proc, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, paused: make(chan string, 1), done: make(chan struct{})}

	var wg sync.WaitGroup
	forward := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			logf("  [%s] %s", name, line)
			if point, ok := strings.CutPrefix(line, fault.PausedMarker+" "); ok {
				select {
				case p.paused <- point:
				default:
				}
			}
		}
	}
	wg.Add(2)
	go forward(stdout)
	go forward(stderr)
	go func() {
		wg.Wait()
		_ = cmd.Wait()
		p.code = cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	return p, nil
}

// waitPaused blocks until the process emits its pause marker.
func (p *proc) waitPaused(ctx context.Context) (string, error) {
	select {
	case point := <-p.paused:
		return point, nil
	case <-p.done:
		return "", fmt.Errorf("%s exited (code %d) before reaching its pause point", p.name, p.code)
	case <-ctx.Done():
		return "", fmt.Errorf("%s: waiting for pause: %w", p.name, ctx.Err())
	}
}

func (p *proc) resume() error {
	return p.cmd.Process.Signal(syscall.SIGUSR1)
}

// wait returns the process exit code, killing it if ctx ends first.
func (p *proc) wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		p.kill()
		return -1, fmt.Errorf("%s did not exit: %w", p.name, ctx.Err())
	}
}

// exitsWithin returns true if the process exits before the deadline.
func (p *proc) exitsWithin(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *proc) kill() {
	select {
	case <-p.done:
	default:
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}
