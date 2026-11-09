//go:build integration

package experiment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/worker"
)

// TestScenarios runs every scenario for both variants and checks the expected result.
func TestScenarios(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("no database configured: %v", err)
	}
	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", binDir+string(os.PathSeparator), "./cmd/conversion-worker", "./cmd/deadline-worker")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workers: %v\n%s", err, out)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ecfg := Config{BinDir: binDir, Lease: 2 * time.Second, Timeout: 60 * time.Second, Log: t.Logf}
	for _, sc := range Scenarios() {
		for _, v := range []worker.Variant{worker.Naive, worker.Safe} {
			t.Run(sc.ID+"/"+string(v), func(t *testing.T) {
				ecfg.Log = t.Logf
				r := Run(ctx, pool, ecfg, sc, v)
				if r.Err != nil {
					t.Fatal(r.Err)
				}
				if !r.Match {
					t.Errorf("health=%s oracle=%s violations=%v, expected GREEN %s (event %s)",
						r.Health.Status, r.Verdict.Overall, r.Verdict.Violations(), r.Expect, r.EventID)
				}
			})
		}
	}
}

func repoRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
