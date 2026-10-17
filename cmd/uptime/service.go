package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/librecosta/uptime/internal/app"
	"github.com/librecosta/uptime/internal/archive"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/internal/service"
)

const serviceHelp = `Usage:
  uptime service list [--config PATH]
  uptime service export <id> [--config PATH] [--output PATH]

List persisted services or export their history as Archive v1 JSON.
bbolt requires an existing database and offline access (stop serve first).
Redis queries can run online but do not provide a snapshot across calls.

Options:
  -h, --help      Show this help
`

const listHelp = `Usage:
  uptime service list [--config PATH]

List persisted IDs, active/detached configuration status, and last-seen UTC time.
Active means configured, not online; detached does not imply writers have stopped.

Options:
  --config PATH   Configuration file (default "./uptime.yaml")
  -h, --help      Show this help
`

const exportHelp = `Usage:
  uptime service export <id> [--config PATH] [--output PATH]

Export all stored daily history and today's sample summary as Archive v1 JSON.
Existing output files are never replaced. This is not a restorable backup.

Options:
  --config PATH   Configuration file (default "./uptime.yaml")
  --output PATH   New file (default "uptime-service-YYYYMMDDTHHMMSSZ.json" in cwd)
  -h, --help      Show this help
`

type serviceArgs struct {
	command, id, configPath, outputPath string
	help                                bool
}

// Explicitly recognize the two value flags so they work on either side of the
// export ID. Values and unknown arguments are never quoted in diagnostics.
func parseServiceArgs(args []string) (serviceArgs, bool) {
	result := serviceArgs{configPath: "./uptime.yaml"}
	if len(args) == 0 || len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		result.help = true
		return result, true
	}
	result.command = args[0]
	if result.command != "list" && result.command != "export" {
		return result, false
	}
	var ids []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			result.help = true
			continue
		}
		key, value, equals := strings.Cut(arg, "=")
		if key == "--config" || key == "--output" && result.command == "export" {
			if !equals {
				i++
				if i == len(args) {
					return result, false
				}
				value = args[i]
			}
			if value == "" {
				return result, false
			}
			if key == "--config" {
				result.configPath = value
			} else {
				result.outputPath = value
			}
			continue
		}
		if strings.HasPrefix(arg, "-") || arg == "" {
			return result, false
		}
		ids = append(ids, arg)
	}
	if result.command == "list" && len(ids) != 0 || result.command == "export" && (len(ids) > 1 || len(ids) == 0 && !result.help) {
		return result, false
	}
	if len(ids) == 1 {
		result.id = ids[0]
	}
	return result, true
}

type serviceSession interface {
	archive.Reader
	Close() error
}

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runService(ctx, args, stdout, stderr, func(ctx context.Context, cfg config.Config) (serviceSession, error) {
		return app.OpenServiceStorage(ctx, cfg)
	})
}

func runService(ctx context.Context, args []string, stdout, stderr io.Writer, open func(context.Context, config.Config) (serviceSession, error)) int {
	options, ok := parseServiceArgs(args)
	if !ok {
		return diagnostic(stderr, "error: invalid service arguments; use 'uptime service --help'", 2)
	}
	if options.help {
		text := serviceHelp
		if options.command == "list" {
			text = listHelp
		} else if options.command == "export" {
			text = exportHelp
		}
		if _, err := io.WriteString(stdout, text); err != nil {
			return diagnostic(stderr, "service: cannot write output", 1)
		}
		return 0
	}
	cfg, err := config.LoadServiceFile(options.configPath)
	if err != nil {
		return diagnostic(stderr, "service: "+err.Error(), 1)
	}
	session, err := open(ctx, cfg)
	if err != nil {
		// OpenServiceStorage returns safe diagnostics, including the offline lock
		// requirement. Never format arbitrary query or filesystem errors below.
		return diagnostic(stderr, "service: "+err.Error(), 1)
	}
	now := time.Now()
	output, document, err := readService(ctx, session, options, cfg, now)
	if err != nil {
		if errors.Is(err, archive.ErrServiceNotFound) {
			return diagnostic(stderr, "service: service not found", 1)
		}
		return diagnostic(stderr, "service: cannot read history or close storage", 1)
	}
	if options.command == "export" {
		path := options.outputPath
		if path == "" {
			path = archive.DefaultFilename(now)
		}
		if err := ctx.Err(); err != nil {
			return diagnostic(stderr, "service: export canceled", 1)
		}
		if err := archive.WriteFile(path, document); err != nil {
			return diagnostic(stderr, "service: cannot publish archive; use a new file in an existing directory supporting hard links", 1)
		}
		output = "service archive exported\n"
	}
	if _, err := io.WriteString(stdout, output); err != nil {
		return diagnostic(stderr, "service: cannot write output", 1)
	}
	return 0
}

// Finish queries and close storage before emitting output or publishing a file.
func readService(ctx context.Context, session serviceSession, options serviceArgs, cfg config.Config, now time.Time) (output string, document archive.Archive, result error) {
	defer func() { result = errors.Join(result, session.Close()) }()
	if options.command == "export" {
		document, result = archive.Build(ctx, session, archive.Options{ID: options.id,
			ApplicationVersion: version, Now: now, Timezone: cfg.Uptime.Timezone, Interval: cfg.Uptime.Interval})
		return
	}
	rows, err := session.ListServices(ctx)
	if err != nil {
		return "", archive.Archive{}, err
	}
	activeIDs := make([]string, 0, len(cfg.Endpoints))
	for _, endpoint := range cfg.Endpoints {
		activeIDs = append(activeIDs, endpoint.ID)
	}
	return service.List(rows, activeIDs), archive.Archive{}, ctx.Err()
}
