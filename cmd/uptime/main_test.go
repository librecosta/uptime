package main

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "no arguments"},
		{name: "command", args: []string{"help"}},
		{name: "short option", args: []string{"-h"}},
		{name: "long option", args: []string{"--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, &stderr)
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %s", &stderr)
			}
			for _, want := range []string{"LibreCosta Uptime", "Usage:", "uptime [command]", "help", "version", "config check", "serve", "service list", "service export", "service remove", "-h", "--help", "--version"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("help missing %q: %s", want, &stdout)
				}
			}
		})
	}
}

func TestVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{arg}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, &stderr)
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr: %s", &stderr)
			}
			for _, want := range []string{"LibreCosta Uptime " + version, "Go: " + runtime.Version(), "Commit: " + commit} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("version missing %q: %s", want, &stdout)
				}
			}
		})
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {""}, {"--unknown"}, {"--version=true"},
		{"config"}, {"config", "unknown"}, {"--config", "uptime.yaml"},
		{"help", "extra"}, {"-h", "extra"}, {"--help", "extra"},
		{"version", "extra"}, {"--version", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("unexpected stdout: %s", &stdout)
			}
			if !strings.Contains(stderr.String(), "error:") || !strings.Contains(stderr.String(), "uptime --help") {
				t.Errorf("stderr must explain the error and point to help: %s", &stderr)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func TestOutputFailure(t *testing.T) {
	for _, command := range []string{"help", "version"} {
		t.Run(command, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(context.Background(), []string{command}, failingWriter{}, &stderr); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), "output unavailable") {
				t.Errorf("missing output error: %s", &stderr)
			}
		})
	}
}
