package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/internal/oracle"
)

func main() {
	eventID := flag.String("event", "", "event id to check (required)")
	grace := flag.Duration("grace", 5*time.Second, "time the system may still converge after the deadline before liveness is judged")
	asJSON := flag.Bool("json", false, "print the verdict as JSON")
	flag.Parse()
	if *eventID == "" {
		flag.Usage()
		os.Exit(2)
	}

	v, err := check(*eventID, *grace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oracle:", err)
		os.Exit(2)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	} else {
		oracle.Print(os.Stdout, v)
	}
	switch v.Overall {
	case oracle.OverallPass:
	case oracle.OverallNotJudged:
		os.Exit(3)
	default:
		os.Exit(1)
	}
}

func check(eventID string, grace time.Duration) (oracle.Verdict, error) {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return oracle.Verdict{}, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 1)
	if err != nil {
		return oracle.Verdict{}, err
	}
	defer pool.Close()
	return oracle.Check(ctx, pool, eventID, grace)
}
