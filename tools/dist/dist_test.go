package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testCommit = "ff0711e21266abadfd603b7c9c11a9fedc71a721"

func TestArguments(t *testing.T) {
	for _, version := range []string{"", "unknown", "../dev", "a..b", "dev/test", `dev\test`, "dev tag", "dev\n", "-X", strings.Repeat("v", 65)} {
		if err := (options{version: version, commit: testCommit, output: "dist"}).validate(); err == nil {
			t.Errorf("accepted version %q", version)
		}
	}
	for _, commit := range []string{"", "unknown", "ff0711e", strings.Repeat("g", 40), testCommit + "\n"} {
		if err := (options{version: "dev", commit: commit, output: "dist"}).validate(); err == nil {
			t.Errorf("accepted SHA %q", commit)
		}
	}
	for _, version := range []string{"dev", "v0.1.0", "v0.1.0-rc.1"} {
		if err := (options{version: version, commit: testCommit, output: "dist"}).validate(); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil || !strings.Contains(stderr.String(), "-commit") {
		t.Fatalf("help: %v, %s", err, &stderr)
	}
	for _, args := range [][]string{{}, {"--unknown"}, {"extra"}, {"--version", "../bad"}, {"--version", "dev", "--commit", testCommit, "--output", ""}} {
		if err := run(context.Background(), args, &stdout, &stderr); err == nil {
			t.Errorf("accepted args %q", args)
		}
	}
}

func TestBuildCommand(t *testing.T) {
	t.Setenv("GOOS", "wrong")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOFLAGS", "-mod=mod")
	opts := options{version: "v0.1.0-rc.1", commit: testCommit}
	for _, target := range targets {
		cmd := buildCommand(context.Background(), "repo with spaces", "binary with spaces", target, opts)
		want := []string{"go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags",
			"-X main.version=v0.1.0-rc.1 -X main.commit=" + testCommit, "-o", "binary with spaces", "./cmd/uptime"}
		if !reflect.DeepEqual(cmd.Args, want) || cmd.Dir != "repo with spaces" {
			t.Fatalf("unexpected command: %+v", cmd)
		}
		for _, want := range []string{"GOOS=" + target.os, "GOARCH=" + target.arch, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS="} {
			key, _, _ := strings.Cut(want, "=")
			count := 0
			for _, value := range cmd.Env {
				if strings.HasPrefix(value, key+"=") {
					count++
					if value != want {
						t.Errorf("environment: got %s, want %s", value, want)
					}
				}
			}
			if count != 1 {
				t.Errorf("%s occurs %d times", key, count)
			}
		}
	}
}

func TestArchivesAndChecksums(t *testing.T) {
	dir := t.TempDir()
	wantNames := []string{
		"librecosta-uptime_dev_darwin_amd64.tar.gz", "librecosta-uptime_dev_darwin_arm64.tar.gz",
		"librecosta-uptime_dev_linux_amd64.tar.gz", "librecosta-uptime_dev_linux_arm64.tar.gz",
		"librecosta-uptime_dev_windows_amd64.zip",
	}
	var wantManifest strings.Builder
	for i, target := range targets {
		if name := target.archive("dev"); name != wantNames[i] {
			t.Fatalf("archive = %s, want %s", name, wantNames[i])
		}
		files := []member{
			{target.binary(), []byte("binary"), 0755},
			{"uptime.example.yaml", []byte("official config\r\n"), 0644},
			{"README.md", []byte("readme\n"), 0644}, {"LICENSE", []byte("original MIT\n"), 0644},
		}
		path := filepath.Join(dir, wantNames[i])
		if err := writeArchive(path, target, files); err != nil {
			t.Fatal(err)
		}
		got, err := readArchive(path, target)
		if err != nil || !reflect.DeepEqual(got, files) {
			t.Fatalf("archive round trip: %v, %+v", err, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&wantManifest, "%x  %s\n", sha256.Sum256(data), wantNames[i])
	}
	if err := writeChecksums(dir, "dev"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil || string(got) != wantManifest.String() {
		t.Fatalf("manifest: %v, %s", err, got)
	}
	if err := verifyChecksums(dir, "dev"); err != nil {
		t.Fatal(err)
	}
	// Even a matching checksum must not make a non-executable a valid distribution.
	if err := verifyDistribution(dir, options{version: "dev"}); err == nil {
		t.Fatal("accepted invalid binaries")
	}
	writeTestFile(t, filepath.Join(dir, wantNames[0]), "tampered archive")
	if err := verifyChecksums(dir, "dev"); err == nil {
		t.Fatal("accepted tampered archive")
	}
}

func TestArchiveRejectsUnexpectedMembers(t *testing.T) {
	for _, target := range []target{{"linux", "amd64"}, {"windows", "amd64"}} {
		for _, bad := range []member{
			{".env", []byte("secret"), 0644}, {"../uptime", nil, 0755},
			{target.binary(), nil, 0644}, {target.binary(), nil, os.ModeSymlink | 0755},
		} {
			path := filepath.Join(t.TempDir(), target.archive("dev"))
			if err := writeArchive(path, target, []member{bad}); err != nil {
				// tar rejects an invalid type/mode combination at write time too.
				continue
			}
			if _, err := readArchive(path, target); err == nil {
				t.Fatalf("accepted unexpected member %+v", bad)
			}
		}
	}
}

func TestBuildFailureAndExistingOutput(t *testing.T) {
	for _, existing := range []string{"absent", "empty", "nonempty", "file"} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "configs"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"README.md", "LICENSE", "configs/uptime.example.yaml"} {
				writeTestFile(t, filepath.Join(root, name), "source")
			}
			out := filepath.Join(root, "dist")
			switch existing {
			case "file":
				writeTestFile(t, out, "keep")
			case "empty", "nonempty":
				if err := os.Mkdir(out, 0755); err != nil {
					t.Fatal(err)
				}
				if existing == "nonempty" {
					writeTestFile(t, filepath.Join(out, "keep"), "keep")
				}
			}
			calls := 0
			failure := errors.New("compiler unavailable")
			err := buildDistribution(context.Background(), root, options{version: "dev", commit: testCommit, output: out},
				func(_ context.Context, _, binary string, _ target, _ options) error {
					calls++
					if calls == 2 {
						return failure
					}
					return os.WriteFile(binary, []byte("binary"), 0755)
				})
			if err == nil {
				t.Fatal("build should fail")
			}
			if existing == "absent" || existing == "empty" {
				if !errors.Is(err, failure) || calls != 2 {
					t.Fatalf("did not reach second target: %v, calls %d", err, calls)
				}
				if _, err := os.Stat(filepath.Join(out, "SHA256SUMS")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed build left a manifest")
				}
			} else {
				if calls != 0 {
					t.Fatal("built before checking existing output")
				}
				keep := out
				if existing == "nonempty" {
					keep = filepath.Join(out, "keep")
				}
				data, err := os.ReadFile(keep)
				if err != nil || string(data) != "keep" {
					t.Fatal("changed existing output")
				}
			}
			stages, err := filepath.Glob(filepath.Join(root, ".uptime-dist-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("staging directory leaked: %v %v", stages, err)
			}
		})
	}
}

func TestRejectSymlinkSourcesAndOutput(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	writeTestFile(t, real, "not allowed through link")
	if err := os.Symlink(real, filepath.Join(root, "LICENSE")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readRegular(root, "LICENSE"); err == nil {
		t.Fatal("followed source symlink")
	}
	if err := checkOutput(filepath.Join(root, "LICENSE")); err == nil {
		t.Fatal("accepted symlink output")
	}
	if _, err := readRegular(root, "../outside"); err == nil {
		t.Fatal("accepted external source")
	}
}

func TestPackagingAndCancellationFailureCleanup(t *testing.T) {
	for _, kind := range []string{"archive", "verification", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "configs"), 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"README.md", "LICENSE", "configs/uptime.example.yaml"} {
				writeTestFile(t, filepath.Join(root, name), "source")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			out := filepath.Join(root, "dist")
			err := buildDistribution(ctx, root, options{version: "dev", commit: testCommit, output: out},
				func(_ context.Context, _, binary string, target target, opts options) error {
					if kind == "canceled" {
						t.Fatal("build invoked after cancellation")
					}
					if kind == "archive" {
						// Force archive creation to fail after a successful build.
						path := filepath.Join(filepath.Dir(binary), "packages", target.archive(opts.version))
						if err := os.Mkdir(path, 0755); err != nil {
							return err
						}
					}
					// Checksums can match, but invalid binaries must still prevent publication.
					return os.WriteFile(binary, []byte("not an executable"), 0755)
				})
			if err == nil || (kind == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("expected %s failure, got %v", kind, err)
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure published output")
			}
			stages, err := filepath.Glob(filepath.Join(root, ".uptime-dist-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("staging directory leaked: %v %v", stages, err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}
