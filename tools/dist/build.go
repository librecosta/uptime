package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type target struct{ os, arch string }

var targets = []target{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"},
}

func (t target) binary() string {
	if t.os == "windows" {
		return "uptime.exe"
	}
	return "uptime"
}

func (t target) archive(version string) string {
	ext := ".tar.gz"
	if t.os == "windows" {
		ext = ".zip"
	}
	return "librecosta-uptime_" + version + "_" + t.os + "_" + t.arch + ext
}

func (o options) ldflags() string {
	return "-X main.version=" + o.version + " -X main.commit=" + o.commit
}

func buildCommand(ctx context.Context, root, binary string, t target, opts options) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
		"-ldflags", opts.ldflags(), "-o", binary, "./cmd/uptime")
	cmd.Dir = root
	// Ignore inherited build/workspace overrides; use this module and target only.
	overrides := []string{"GOOS=" + t.os, "GOARCH=" + t.arch, "CGO_ENABLED=0",
		"GOWORK=off", "GOFLAGS=", "GOAMD64=v1", "GOARM64=v8.0"}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		overridden := false
		for _, override := range overrides {
			name, _, _ := strings.Cut(override, "=")
			if strings.EqualFold(key, name) {
				overridden = true
				break
			}
		}
		if !overridden {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, overrides...)
	return cmd
}

func buildBinary(ctx context.Context, root, binary string, t target, opts options) error {
	cmd := buildCommand(ctx, root, binary, t, opts)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func buildDistribution(ctx context.Context, root string, opts options,
	build func(context.Context, string, string, target, options) error,
) (err error) {
	if err := opts.validate(); err != nil {
		return err
	}
	output, err := filepath.Abs(opts.output)
	if err != nil {
		return err
	}
	if err := checkOutput(output); err != nil {
		return err
	}
	// A sibling staging directory makes the final directory rename stay on one filesystem.
	// Only this uniquely owned directory is ever recursively removed.
	stage, err := os.MkdirTemp(filepath.Dir(output), ".uptime-dist-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	packages := filepath.Join(stage, "packages")
	if err := os.Mkdir(packages, 0755); err != nil {
		return err
	}
	docs := make([]member, 0, 3)
	// Keep a fixed source allowlist rather than walking the repository: local
	// configurations, credentials and monitoring data must never enter an archive.
	for _, file := range []struct{ source, name string }{
		{"configs/uptime.example.yaml", "uptime.example.yaml"},
		{"README.md", "README.md"}, {"LICENSE", "LICENSE"},
	} {
		data, err := readRegular(root, file.source)
		if err != nil {
			return err
		}
		docs = append(docs, member{file.name, data, 0644})
	}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		binary := filepath.Join(stage, t.binary())
		if err := build(ctx, root, binary, t, opts); err != nil {
			return fmt.Errorf("build %s/%s: %w", t.os, t.arch, err)
		}
		data, err := readRegular(stage, t.binary())
		if err != nil {
			return err
		}
		files := append([]member{{t.binary(), data, 0755}}, docs...)
		if err := writeArchive(filepath.Join(packages, t.archive(opts.version)), t, files); err != nil {
			return err
		}
	}
	if err := writeChecksums(packages, opts.version); err != nil {
		return err
	}
	// A successful compiler invocation alone does not make a complete distribution.
	// Verify all five archives and checksums before the final path becomes visible.
	if err := verifyDistribution(packages, opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Recheck before publication and remove the checked empty directory without recursion.
	if err := checkOutput(output); err != nil {
		return err
	}
	if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(packages, output)
}

func checkOutput(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be an absent or empty directory, not a file or symlink")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output directory is not empty; existing files were not changed")
	}
	return nil
}

// readRegular admits only explicit relative files, with no symlink path components.
func readRegular(root, name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, errors.New("non-local package source")
	}
	path := root
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink package source: %s", name)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("non-regular package source: %s", name)
	}
	return os.ReadFile(path)
}
