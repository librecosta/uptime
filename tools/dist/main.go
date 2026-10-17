// Command dist builds and verifies the five supported distribution archives.
// Builds require the repository root, an explicit version and a full Git commit.
// It creates local preview artifacts only, without tags, releases or registry uploads.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
)

type options struct {
	version, commit, output string
	verify                  bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dist:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var opts options
	flags := flag.NewFlagSet("dist", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.version, "version", "", "filename-safe version (required, e.g. dev)")
	flags.StringVar(&opts.commit, "commit", "", "full 40-digit Git SHA (required when building)")
	flags.StringVar(&opts.output, "output", "dist", "output directory; must be absent or empty when building")
	flags.BoolVar(&opts.verify, "verify", false, "verify existing archives and SHA256SUMS without rebuilding")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := opts.validate(); err != nil {
		return err
	}
	if opts.verify {
		if err := verifyDistribution(opts.output, opts); err != nil {
			return err
		}
	} else {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := buildDistribution(ctx, root, opts, buildBinary); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(stdout, "Verified five distribution archives and SHA256SUMS in %s\n", opts.output)
	return err
}

func (o options) validate() error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(o.version) ||
		strings.Contains(o.version, "..") || o.version == "unknown" {
		return errors.New("version must be 1..64 filename-safe ASCII characters, without '..' or 'unknown'")
	}
	if !(o.verify && o.commit == "") && !regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(o.commit) {
		return errors.New("commit must be a full 40-digit hexadecimal Git SHA")
	}
	if strings.TrimSpace(o.output) == "" {
		return errors.New("output directory is required")
	}
	return nil
}
