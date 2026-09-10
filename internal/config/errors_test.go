package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The error message is the operator-facing surface of this package: it is
// read once every few months, by someone who has forgotten the schema.
func TestErrorMessageListsEveryProblem(t *testing.T) {
	one := &Error{Problems: []string{"floor_kbps: required"}}
	if got := one.Error(); got != "floor_kbps: required" {
		t.Errorf("a single problem should read as one line, got %q", got)
	}

	many := &Error{Source: "config.yaml", Problems: []string{"first", "second"}}
	got := many.Error()
	for _, want := range []string{"config.yaml", "2 problems", "- first", "- second"} {
		if !strings.Contains(got, want) {
			t.Errorf("message is missing %q:\n%s", want, got)
		}
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(replace(t, "        floor_kbps: 3000\n", "")), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("got %T, want *config.Error", err)
	}
	if ce.Source != path {
		t.Errorf("Source = %q, want %q", ce.Source, path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the filename should be in the message:\n%v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

// A profile written as something other than a mapping is a plausible typo
// (a stray dash makes it a list) and must not panic.
func TestMalformedProfileIsReported(t *testing.T) {
	wantProblem(t, replace(t, "libraries:", "  broken: just-a-string\nlibraries:"), "profiles.broken")
}

func TestMalformedDocumentIsReported(t *testing.T) {
	if _, err := Parse([]byte("defaults:\n  profile: [unclosed\n")); err == nil {
		t.Fatal("expected a parse error for malformed YAML")
	}
}
