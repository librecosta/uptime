package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/librecosta/uptime/internal/archive"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
)

func TestServiceRemoveActiveBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, storage := range []string{"", "storage: {type: redis, redis: {url: 'redis://user:SECRET@never-resolves.invalid'}}\n"} {
		writeConfig(t, "uptime.yaml", validConfig+storage)
		var stdout, stderr bytes.Buffer
		code := runService(context.Background(), []string{"remove", "web"}, &stdout, &stderr, func(context.Context, config.Config) (serviceSession, error) {
			t.Fatal("active removal opened storage")
			return nil, nil
		})
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "still configured") || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("active guard: %d %q %q", code, &stdout, &stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	requireNoError(t, err)
	if len(entries) != 1 {
		t.Fatal("active refusal created files")
	}
}

func storedServiceArchive(t *testing.T, path, id string, now time.Time) archive.Archive {
	t.Helper()
	store, err := bbolt.Open(bbolt.Config{Path: path, MustExist: true})
	requireNoError(t, err)
	defer func() { requireNoError(t, store.Close()) }()
	doc, err := archive.Build(context.Background(), store, archive.Options{ID: id, Now: now, Timezone: time.UTC, Interval: time.Second})
	requireNoError(t, err)
	return doc
}

func TestServiceRemoveBbolt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", validConfig)
	path := filepath.Join(dir, "data", "uptime.db")
	seedCLIHistory(t, path)
	before := historySnapshot(t, path)
	for _, tc := range []struct{ id, want string }{{"web", "still configured"}, {"absent-SECRET\n\x1b[31m", "not found"}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"service", "remove", tc.id}, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) || strings.ContainsAny(stderr.String(), "\x1b") || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("refusal: %d %q %q", code, &stdout, &stderr)
		}
		if after := historySnapshot(t, path); after != before {
			t.Fatal("refused removal changed history")
		}
	}
	now := time.Now().UTC()
	keep := storedServiceArchive(t, path, "web", now)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"service", "remove", "retired", "--config", "uptime.yaml"}, &stdout, &stderr); code != 0 || stdout.String() != "service removed\n" || stderr.Len() != 0 {
		t.Fatalf("remove: %d %q %q", code, &stdout, &stderr)
	}
	if after := storedServiceArchive(t, path, "web", now); !reflect.DeepEqual(keep, after) {
		t.Fatal("other service metadata/daily/samples changed")
	}
	store, err := bbolt.Open(bbolt.Config{Path: path, MustExist: true})
	requireNoError(t, err)
	rows, err := store.ListServices(context.Background())
	requireNoError(t, err)
	daily, err := store.QueryDaily(context.Background(), uptimestorage.QueryDailyOptions{ServiceIDs: []string{"retired"}})
	requireNoError(t, err)
	samples, err := store.QueryTodaySamples(context.Background(), uptimestorage.QueryTodaySamplesOptions{ServiceIDs: []string{"retired"}, Day: now.Format(time.DateOnly)})
	requireNoError(t, err)
	requireNoError(t, store.Close())
	if len(rows) != 1 || rows[0].ID != "web" || len(daily) != 0 || len(samples) != 0 {
		t.Fatal("service or history remains after removal")
	}
	// B1 list/export remain usable after deleting a different service.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "list"}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "retired") || !strings.Contains(stdout.String(), "active") {
		t.Fatalf("list after removal: %d %s", code, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "export", "web", "--output", "keep.json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("export after removal: %d %s", code, &stderr)
	}
	data, err := os.ReadFile("keep.json")
	requireNoError(t, err)
	if !json.Valid(data) {
		t.Fatal("invalid surviving archive")
	}
	writeConfig(t, "uptime.yaml", "endpoints: []\n")
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "remove", "--config=uptime.yaml", "web"}, &stdout, &stderr); code != 0 || stdout.String() != "service removed\n" {
		t.Fatalf("last service: %d %s", code, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "list"}, &stdout, &stderr); code != 0 || strings.Join(strings.Fields(stdout.String()), " ") != "ID STATUS LAST SEEN (UTC)" {
		t.Fatal("last service still listed")
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "remove", "web"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "not found") {
		t.Fatal("repeat removal reported success")
	}
	entries, err := os.ReadDir(dir)
	requireNoError(t, err)
	if len(entries) != 3 {
		t.Fatalf("removal created unexpected files: %v", entries)
	}
}

func TestServiceRemoveBboltLockAndMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "uptime.db")
	configPath := filepath.Join(dir, "uptime.yaml")
	writeConfig(t, configPath, fmt.Sprintf("storage: {bbolt: {path: %q}}\nendpoints: []", path))
	args := []string{"service", "remove", "retired", "--config", configPath}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "existing bbolt database") {
		t.Fatalf("missing file: %d %s", code, &stderr)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal created an empty database/directory")
	}
	seedCLIHistory(t, path)
	owner, err := bbolt.Open(bbolt.Config{Path: path})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, owner.Close()) })
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "stop serve") {
		t.Fatalf("lock bypassed: %d %s", code, &stderr)
	}
	rows, err := owner.ListServices(context.Background())
	requireNoError(t, err)
	if len(rows) != 2 {
		t.Fatal("locked removal changed records")
	}
	requireNoError(t, owner.Close())
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), args, &stdout, &stderr); code != 0 || stdout.String() != "service removed\n" {
		t.Fatalf("offline removal: %d %s", code, &stderr)
	}
}

type removalSession struct {
	archive.Reader               // Queries other than ListServices must not be used by removal.
	rows                         []uptimestorage.Service
	listErr, removeErr, closeErr error
	failListAt, lists, closes    int
	removed                      []string
	retain                       bool
	cancelAfterList              context.CancelFunc
}

func (s *removalSession) ListServices(context.Context) ([]uptimestorage.Service, error) {
	s.lists++
	if s.cancelAfterList != nil {
		s.cancelAfterList()
	}
	if s.lists == s.failListAt {
		return nil, s.listErr
	}
	return s.rows, nil
}
func (s *removalSession) RemoveService(_ context.Context, id string) error {
	s.removed = append(s.removed, id)
	if s.removeErr != nil {
		return s.removeErr
	}
	if !s.retain {
		s.rows = slices.DeleteFunc(s.rows, func(row uptimestorage.Service) bool { return row.ID == id })
	}
	return nil
}
func (s *removalSession) Close() error { s.closes++; return s.closeErr }

type afterRemovalCloseWriter struct {
	t       *testing.T
	session *removalSession
	output  io.Writer
}

func (w afterRemovalCloseWriter) Write(data []byte) (int, error) {
	if w.session.closes != 1 {
		w.t.Fatal("success output preceded storage Close")
	}
	return w.output.Write(data)
}

func TestServiceRemoveFailuresAndClose(t *testing.T) {
	t.Chdir(t.TempDir())
	writeConfig(t, "uptime.yaml", "endpoints: []")
	cause := errors.New("redis://user:SECRET@host")
	for _, failure := range []string{"missing", "list", "remove", "readback", "recreated", "close", "canceled", "cancel-after-list", "stdout", "success"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			session := &removalSession{rows: []uptimestorage.Service{{ID: "retired"}}, listErr: cause}
			wantRemovals, wantLists, wantCode := 1, 2, 1
			switch failure {
			case "missing":
				session.rows = nil
				wantRemovals, wantLists = 0, 1
			case "list":
				session.failListAt = 1
				wantRemovals, wantLists = 0, 1
			case "remove":
				session.removeErr = cause
				wantLists = 1
			case "readback":
				session.failListAt = 2
			case "recreated":
				session.retain = true
			case "close":
				session.closeErr = cause
			case "canceled":
				cancel()
				wantRemovals, wantLists = 0, 0
			case "cancel-after-list":
				session.cancelAfterList = cancel
				wantRemovals, wantLists = 0, 1
			case "success":
				wantCode = 0
			}
			var stdout, stderr bytes.Buffer
			var output io.Writer = &stdout
			if failure == "stdout" {
				output = failingWriter{}
			}
			code := runService(ctx, []string{"remove", "retired"}, afterRemovalCloseWriter{t, session, output}, &stderr, func(context.Context, config.Config) (serviceSession, error) { return session, nil })
			if code != wantCode || session.closes != 1 || len(session.removed) != wantRemovals || session.lists != wantLists || strings.Contains(stderr.String(), "SECRET") {
				t.Fatalf("code=%d lists=%d removals=%v closes=%d stderr=%q", code, session.lists, session.removed, session.closes, &stderr)
			}
			if wantCode != 0 && (stdout.Len() != 0 || stderr.Len() == 0) {
				t.Fatal("failed removal reported success")
			}
			if wantCode == 0 && (stdout.String() != "service removed\n" || stderr.Len() != 0) {
				t.Fatal("missing success result")
			}
			if wantRemovals == 1 && session.removed[0] != "retired" {
				t.Fatal("removed wrong ID")
			}
		})
	}
	closeCause := errors.New("close SECRET")
	session := &removalSession{rows: []uptimestorage.Service{{ID: "retired"}}, removeErr: cause, closeErr: closeCause}
	_, _, err := operateService(context.Background(), session, serviceArgs{command: "remove", id: "retired"}, config.Config{}, time.Now())
	if !errors.Is(err, cause) || !errors.Is(err, closeCause) || session.closes != 1 || len(session.removed) != 1 {
		t.Fatal("removal/close causes lost or deletion retried")
	}
}
