package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/experiment"
	"waitlist-chaos/internal/worker"
)

func main() {
	scenario := flag.String("scenario", "all", "scenario id (s0..s4) or all")
	variant := flag.String("variant", "both", "naive, safe or both")
	binDir := flag.String("bin", "bin", "directory containing the worker binaries")
	lease := flag.Duration("lease", 2*time.Second, "conversion job lease")
	timeout := flag.Duration("timeout", 60*time.Second, "timeout per scenario run")
	verbose := flag.Bool("v", false, "print worker output")
	flag.Parse()

	if err := run(*scenario, *variant, *binDir, *lease, *timeout, *verbose); err != nil {
		log.Fatal(err)
	}
}

func run(scenarioID, variant, binDir string, lease, timeout time.Duration, verbose bool) error {
	scenarios := experiment.Select(scenarioID)
	if len(scenarios) == 0 {
		return fmt.Errorf("unknown scenario %q", scenarioID)
	}
	variants := []worker.Variant{worker.Naive, worker.Safe}
	if variant != "both" {
		v, err := worker.ParseVariant(variant)
		if err != nil {
			return err
		}
		variants = []worker.Variant{v}
	}
	for _, b := range []string{experiment.ConversionBin, experiment.DeadlineBin} {
		if _, err := os.Stat(filepath.Join(binDir, b)); err != nil {
			return fmt.Errorf("worker binary %s not found in %s (run `make build`)", b, binDir)
		}
	}

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	logf := func(string, ...any) {}
	if verbose {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	ecfg := experiment.Config{BinDir: binDir, Lease: lease, Timeout: timeout, Log: logf}

	var results []experiment.Result
	for _, sc := range scenarios {
		for _, v := range variants {
			fmt.Fprintf(os.Stderr, "== %s/%s: %s\n", sc.ID, v, sc.Title)
			r := experiment.Run(ctx, pool, ecfg, sc, v)
			fmt.Fprintf(os.Stderr, "   done in %s\n", r.Duration.Round(time.Millisecond))
			results = append(results, r)
		}
	}
	fmt.Println()
	experiment.PrintReport(os.Stdout, results)

	for _, r := range results {
		if !r.Match {
			return fmt.Errorf("%s/%s did not match its expectation", r.Scenario, r.Variant)
		}
	}
	return nil
}
