// Command uptime provides the LibreCosta Uptime CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/gofiber/fiber/v3/log"
	"github.com/librecosta/uptime/internal/app"
	"github.com/librecosta/uptime/internal/config"
)

var (
	version = "dev"
	commit  = "unknown"
)

const helpText = `LibreCosta Uptime

Usage:
  uptime [command]

Commands:
  help          Show this help
  version       Show version information
  config check  Validate configuration
  serve         Run the uptime service

Options:
  -h, --help    Show this help
  --version     Show version information

No arguments shows this help.
`

func main() {
	log.SetOutput(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		return serve(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "config" {
		if len(args) < 2 || args[1] != "check" {
			return diagnostic(stderr, "error: expected 'uptime config check'; use 'uptime --help'", 2)
		}
		return checkConfig(args[2:], stdout, stderr)
	}
	if len(args) > 1 {
		return diagnostic(stderr, "error: unexpected arguments; use 'uptime --help'", 2)
	}

	command := "help"
	if len(args) == 1 {
		command = args[0]
	}

	var err error
	switch command {
	case "help", "-h", "--help":
		_, err = io.WriteString(stdout, helpText)
	case "version", "--version":
		_, err = fmt.Fprintf(stdout, "LibreCosta Uptime %s\nGo: %s\nCommit: %s\n", version, runtime.Version(), commit)
	default:
		return diagnostic(stderr, "error: unknown command or option; use 'uptime --help'", 2)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: write output: %v\n", err)
		return 1
	}
	return 0
}

const configHelp = `Usage:
  uptime config check [--config PATH]

Validate an Uptime configuration without starting the service.

Options:
  --config PATH   Configuration file (default "./uptime.yaml")
  -h, --help      Show this help
`

func checkConfig(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("config check", flag.ContinueOnError)
	// Flag errors can quote supplied values. Emit our own safe usage diagnostic.
	flags.SetOutput(io.Discard)
	path := flags.String("config", "./uptime.yaml", "configuration file")
	var help bool
	flags.BoolVar(&help, "h", false, "show help")
	flags.BoolVar(&help, "help", false, "show help")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return diagnostic(stderr, "error: invalid arguments; use 'uptime config check --help'", 2)
	}
	output := configHelp
	if !help {
		if _, err := config.LoadFile(*path); err != nil {
			return diagnostic(stderr, "config check: "+err.Error(), 1)
		}
		output = "configuration is valid\n"
	}
	if _, err := io.WriteString(stdout, output); err != nil {
		return diagnostic(stderr, "config check: cannot write output", 1)
	}
	return 0
}

func diagnostic(stderr io.Writer, message string, code int) int {
	if _, err := fmt.Fprintln(stderr, message); err != nil {
		return 1
	}
	return code
}

const serveHelp = `Usage:
  uptime serve [--config PATH]

Run the uptime service with bbolt or Redis storage, HTTP/HTTPS, and optional Basic Auth.

Options:
  --config PATH   Configuration file (default "./uptime.yaml")
  -h, --help      Show this help
`

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "./uptime.yaml", "configuration file")
	var help bool
	flags.BoolVar(&help, "h", false, "show help")
	flags.BoolVar(&help, "help", false, "show help")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return diagnostic(stderr, "error: invalid arguments; use 'uptime serve --help'", 2)
	}
	if help {
		if _, err := io.WriteString(stdout, serveHelp); err != nil {
			return diagnostic(stderr, "serve: cannot write output", 1)
		}
		return 0
	}
	cfg, err := config.LoadFile(*path)
	if err != nil {
		return diagnostic(stderr, "serve: "+err.Error(), 1)
	}
	server, err := app.New(cfg)
	if err != nil {
		return diagnostic(stderr, "serve: "+err.Error(), 1)
	}
	if err := server.Run(ctx); err != nil {
		return diagnostic(stderr, "serve: "+err.Error(), 1)
	}
	return 0
}
