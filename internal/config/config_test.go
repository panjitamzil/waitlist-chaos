package config

import (
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	in := `
# comment
DATABASE_URL=postgresql://u:p@h:5432/db?x=1
export QUOTED="hello world"
SINGLE='a=b'
`
	got, err := ParseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DATABASE_URL": "postgresql://u:p@h:5432/db?x=1",
		"QUOTED":       "hello world",
		"SINGLE":       "a=b",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseDotEnvInvalidLine(t *testing.T) {
	if _, err := ParseDotEnv(strings.NewReader("NOEQUALS")); err == nil {
		t.Fatal("expected error")
	}
}
