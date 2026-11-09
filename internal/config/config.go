package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	DatabaseURL string
}

// Load reads .env from the working directory upward and returns configuration.
func Load() (Config, error) {
	if path, ok := findDotEnv(); ok {
		if err := LoadDotEnv(path); err != nil {
			return Config{}, err
		}
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("DATABASE_URL is not set (copy .env.example to .env)")
	}
	return Config{DatabaseURL: url}, nil
}

func findDotEnv() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if p := filepath.Join(dir, ".env"); exists(p) {
			return p, true
		}
		parent := filepath.Dir(dir)
		if exists(filepath.Join(dir, "go.mod")) || parent == dir {
			return "", false
		}
		dir = parent
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// LoadDotEnv sets env vars from path without overriding existing ones.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	vars, err := ParseDotEnv(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseDotEnv parses KEY=VALUE lines from r, ignoring blanks and comments.
func ParseDotEnv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}
