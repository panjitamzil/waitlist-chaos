package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"waitlist-chaos/internal/config"
	"waitlist-chaos/internal/db"
	"waitlist-chaos/migrations"
)

func main() {
	flag.Usage = func() { fmt.Fprintln(os.Stderr, "usage: migrate up|down|status") }
	flag.Parse()
	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "up"
	}
	if err := run(context.Background(), cmd); err != nil {
		log.Fatalf("migrate %s: %v", cmd, err)
	}
}

func run(ctx context.Context, cmd string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 1)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE schema_migrations ENABLE ROW LEVEL SECURITY;`); err != nil {
		return err
	}

	switch cmd {
	case "up":
		return up(ctx, pool)
	case "down":
		return down(ctx, pool)
	case "status":
		return status(ctx, pool)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func versions() ([]string, error) {
	files, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, strings.TrimSuffix(f, ".up.sql"))
	}
	sort.Strings(out)
	return out, nil
}

func applied(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	vs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(vs))
	for _, v := range vs {
		out[v] = true
	}
	return out, nil
}

func up(ctx context.Context, pool *pgxpool.Pool) error {
	all, err := versions()
	if err != nil {
		return err
	}
	done, err := applied(ctx, pool)
	if err != nil {
		return err
	}
	n := 0
	for _, v := range all {
		if done[v] {
			continue
		}
		sql, err := fs.ReadFile(migrations.FS, v+".up.sql")
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		fmt.Println("applied", v)
		n++
	}
	if n == 0 {
		fmt.Println("no pending migrations")
	}
	return nil
}

func down(ctx context.Context, pool *pgxpool.Pool) error {
	var v string
	err := pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&v)
	if err == pgx.ErrNoRows {
		fmt.Println("nothing to roll back")
		return nil
	}
	if err != nil {
		return err
	}
	sql, err := fs.ReadFile(migrations.FS, v+".down.sql")
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, v)
		return err
	})
	if err != nil {
		return fmt.Errorf("%s: %w", v, err)
	}
	fmt.Println("rolled back", v)
	return nil
}

func status(ctx context.Context, pool *pgxpool.Pool) error {
	all, err := versions()
	if err != nil {
		return err
	}
	done, err := applied(ctx, pool)
	if err != nil {
		return err
	}
	for _, v := range all {
		state := "pending"
		if done[v] {
			state = "applied"
		}
		fmt.Printf("%-20s %s\n", v, state)
	}
	return nil
}
