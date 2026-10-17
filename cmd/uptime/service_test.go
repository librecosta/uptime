package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/librecosta/uptime/internal/archive"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
)

func TestServiceHelpAndArguments(t *testing.T) {
	t.Chdir(t.TempDir()) // No config or database: help must not need either.
	for _, args := range [][]string{
		{"service"}, {"service", "--help"}, {"service", "-h"},
		{"service", "list", "--help"}, {"service", "export", "-h"},
		{"service", "export", "web", "--config", "missing.yaml", "--help"},
		{"service", "list", "--config", "missing.yaml", "-h"},
		{"service", "remove", "--help"},
		{"service", "remove", "web", "--config", "missing.yaml", "-h"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "Usage:") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, &stdout, &stderr)
		}
	}
	for _, args := range [][]string{
		{"service", "remove"}, {"service", "unknown-SECRET"}, {"service", "list", "web"},
		{"service", "remove", "web", "extra"}, {"service", "remove", ""},
		{"service", "remove", "web", "--output", "SECRET"},
		{"service", "remove", "web", "--force"}, {"service", "remove", "web", "--yes"},
		{"service", "remove", "web", "--online"}, {"service", "remove", "web", "--config"},
		{"service", "export"}, {"service", "export", "web", "extra"}, {"service", "export", ""},
		{"service", "list", "--config"}, {"service", "export", "web", "--output"},
		{"service", "list", "--output", "SECRET"}, {"service", "export", "web", "--output="},
		{"service", "export", "web", "--force"}, {"service", "list", "--yes"},
		{"service", "list", "--online"}, {"service", "list", "--config="},
		{"service", "export", "--help", "--unknown=SECRET"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--help") || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, &stdout, &stderr)
		}
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func seedCLIHistory(t *testing.T, path string) {
	t.Helper()
	store, err := bbolt.Open(bbolt.Config{Path: path})
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, store.Close()) })
	ctx := context.Background()
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -70)
	for i, id := range []string{"web", "retired"} {
		requireNoError(t, store.UpsertService(ctx, uptimestorage.Service{ID: id, Name: "Stored " + id, Description: "Historical service", CreatedAt: created, LastSeenAt: now, SampleInterval: time.Minute}))
		requireNoError(t, store.UpsertInstance(ctx, uptimestorage.Instance{ID: int64(i + 1), ServiceID: id, Hostname: "SECRET-HOST", PID: 12345, StartedAt: created}))
		for _, day := range []string{created.Format(time.DateOnly), created.AddDate(0, 0, 2).Format(time.DateOnly), now.Format(time.DateOnly)} {
			requireNoError(t, store.WriteHeartbeat(ctx, uptimestorage.Heartbeat{ServiceID: id, InstanceID: int64(i + 1), Day: day, Slot: 0, SeenAt: now}))
		}
	}
	requireNoError(t, store.RollupDaily(ctx, uptimestorage.RollupOptions{BeforeDay: now.Format(time.DateOnly), ExpectedSlots: func(uptimestorage.Service, string) int { return 1440 }}))
	requireNoError(t, store.Close())
}

func historySnapshot(t *testing.T, path string) string {
	t.Helper()
	store, err := bbolt.Open(bbolt.Config{Path: path})
	requireNoError(t, err)
	defer func() { requireNoError(t, store.Close()) }()
	services, err := store.ListServices(context.Background())
	requireNoError(t, err)
	daily, err := store.QueryDaily(context.Background(), uptimestorage.QueryDailyOptions{})
	requireNoError(t, err)
	samples, err := store.QueryTodaySamples(context.Background(), uptimestorage.QueryTodaySamplesOptions{Day: time.Now().UTC().Format(time.DateOnly)})
	requireNoError(t, err)
	data, err := json.Marshal([]any{services, daily, samples})
	requireNoError(t, err)
	return string(data)
}

func TestServiceBboltListExport(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer target.Close()
	// An occupied listening address proves these commands do not start HTTP.
	writeConfig(t, "uptime.yaml", fmt.Sprintf("server: {address: %q}\nendpoints: [{id: web, url: '%s?token=SECRET-URL', headers: {Authorization: 'SECRET-HEADER'}}]\n", target.Listener.Addr().String(), target.URL))
	database := filepath.Join(dir, "data", "uptime.db")
	empty, err := bbolt.Open(bbolt.Config{Path: database})
	requireNoError(t, err)
	requireNoError(t, empty.Close())
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"service", "list"}, &stdout, &stderr); code != 0 || strings.Join(strings.Fields(stdout.String()), " ") != "ID STATUS LAST SEEN (UTC)" || stderr.Len() != 0 {
		t.Fatalf("empty list code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	stdout.Reset()
	seedCLIHistory(t, database)
	before := historySnapshot(t, database)
	if code := run(context.Background(), []string{"service", "list"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("list code=%d stderr=%q", code, &stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(strings.Join(strings.Fields(lines[1]), " "), "retired detached ") || !strings.HasPrefix(strings.Join(strings.Fields(lines[2]), " "), "web active ") || !strings.HasSuffix(lines[2], "Z") {
		t.Fatalf("list output: %q", &stdout)
	}
	for _, id := range []string{"retired", "web"} {
		stdout.Reset()
		stderr.Reset()
		args := []string{"service", "export", id, "--config", "uptime.yaml", "--output", id + ".json"}
		if id == "web" {
			args = []string{"service", "export", "--config=uptime.yaml", id, "--output=" + id + ".json"}
		}
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 || stdout.String() != "service archive exported\n" || stderr.Len() != 0 {
			t.Fatalf("export code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
		}
		data, err := os.ReadFile(id + ".json")
		requireNoError(t, err)
		var document archive.Archive
		requireNoError(t, json.Unmarshal(data, &document))
		if document.Version != 1 || document.ApplicationVersion != version || document.Service.ID != id || document.Service.Name != "Stored "+id || document.Service.SampleInterval != "1m0s" || len(document.Daily) != 2 || document.Daily[0].Day >= document.Daily[1].Day || document.Daily[0].UpSlots != 1 || document.Daily[0].ExpectedSlots != 1440 || !document.Daily[0].Finalized || document.CurrentDay.UpSlots != 1 {
			t.Fatalf("incomplete history: %s", data)
		}
		for _, secret := range []string{"SECRET", "Authorization", "hostname", "instance", "pid", "url", "headers", "auth", "tls"} {
			if strings.Contains(strings.ToLower(string(data)), strings.ToLower(secret)) {
				t.Fatalf("archive contains forbidden field/value %q", secret)
			}
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "export", "retired"}, &stdout, &stderr); code != 0 {
		t.Fatalf("default export: %d %s", code, &stderr)
	}
	files, err := filepath.Glob("uptime-service-*.json")
	requireNoError(t, err)
	if len(files) != 1 {
		t.Fatalf("default filenames: %v", files)
	}
	if after := historySnapshot(t, database); after != before || requests.Load() != 0 {
		t.Fatal("management changed source history or probed an endpoint")
	}

	// Explicitly retiring the final endpoint changes only the derived status.
	writeConfig(t, "uptime.yaml", "endpoints: []\n")
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "list"}, &stdout, &stderr); code != 0 || strings.Count(stdout.String(), "detached") != 2 {
		t.Fatalf("maintenance list: %d %s", code, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"service", "export", "retired", "--output", "maintenance.json"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("maintenance export: %d %s", code, &stderr)
	}
	for _, args := range [][]string{{"serve"}, {"config", "check"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "endpoints") {
			t.Fatalf("empty endpoints accepted by %v: %d", args, code)
		}
	}
}

func TestServiceFailuresDoNotPublish(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", "endpoints: []\n")
	for _, command := range [][]string{{"service", "list"}, {"service", "export", "missing", "--output", "out.json"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), command, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "existing bbolt database") {
			t.Fatalf("missing database: %d %s", code, &stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	requireNoError(t, err)
	if len(entries) != 1 {
		t.Fatal("missing database created files/directories")
	}
	seedCLIHistory(t, filepath.Join(dir, "data", "uptime.db"))
	writeConfig(t, "keep.json", "keep")
	for _, tc := range []struct{ id, output, want string }{
		{"missing-SECRET", "out.json", "not found"},
		{"web", "keep.json", "cannot publish"},
		{"web", "SECRET/out.json", "cannot publish"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"service", "export", tc.id, "--output", tc.output}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("export failure: %d %s", code, &stderr)
		}
	}
	entries, err = os.ReadDir(dir)
	requireNoError(t, err)
	if len(entries) != 3 {
		t.Fatalf("failed export left files: %v", entries)
	}
	data, err := os.ReadFile("keep.json")
	requireNoError(t, err)
	if string(data) != "keep" {
		t.Fatal("existing output was replaced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"service", "export", "web", "--output", "out.json"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatalf("canceled export: %d", code)
	}
	// Missing endpoints and config still fail even in maintenance mode.
	writeConfig(t, "uptime.yaml", "ui: {}")
	for _, args := range [][]string{{"service", "list"}, {"service", "list", "--config", "SECRET.yaml"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("invalid configuration: %d %s", code, &stderr)
		}
	}
}

type failedServiceSession struct {
	queryErr, closeErr error
	closes, queries    int
}

func (s *failedServiceSession) ListServices(context.Context) ([]uptimestorage.Service, error) {
	s.queries++
	return []uptimestorage.Service{{ID: "web"}}, s.queryErr
}
func (s *failedServiceSession) QueryDaily(context.Context, uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	s.queries++
	return nil, s.queryErr
}
func (s *failedServiceSession) QueryTodaySamples(context.Context, uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	s.queries++
	return nil, s.queryErr
}
func (s *failedServiceSession) Close() error { s.closes++; return s.closeErr }
func (s *failedServiceSession) RemoveService(context.Context, string) error {
	return errors.New("unexpected removal during list/export")
}

func TestServiceClosesBeforeOutputAndPreservesErrors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", "endpoints: []")
	cause, closeCause := errors.New("redis://SECRET@host"), errors.New("close SECRET")
	for _, command := range [][]string{{"list"}, {"export", "web", "--output", "out.json"}} {
		for _, queryErr := range []error{nil, cause} {
			session := &failedServiceSession{queryErr: queryErr, closeErr: closeCause}
			var stdout, stderr bytes.Buffer
			code := runService(context.Background(), command, &stdout, &stderr, func(context.Context, config.Config) (serviceSession, error) { return session, nil })
			if code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SECRET") || session.closes != 1 {
				t.Fatalf("unsafe failure or missing cleanup: %d %s", code, &stderr)
			}
		}
	}
	if _, err := os.Stat("out.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published despite Close failure")
	}
	session := &failedServiceSession{queryErr: cause, closeErr: closeCause}
	_, _, err := operateService(context.Background(), session, serviceArgs{command: "list"}, config.Config{}, time.Now())
	if !errors.Is(err, cause) || !errors.Is(err, closeCause) || session.closes != 1 {
		t.Fatal("operation/close error chain lost")
	}
	for _, args := range [][]string{{"service", "--help"}, {"service", "list", "-h"}, {"service", "export", "-h"}, {"service", "remove", "-h"}} {
		if code := run(context.Background(), args, failingWriter{}, io.Discard); code != 1 {
			t.Fatal("help write failure ignored")
		}
	}
	session = &failedServiceSession{}
	if code := runService(context.Background(), []string{"list"}, failingWriter{}, io.Discard, func(context.Context, config.Config) (serviceSession, error) { return session, nil }); code != 1 || session.closes != 1 {
		t.Fatal("list output failure ignored or session leaked")
	}
	session = &failedServiceSession{}
	if code := runService(context.Background(), []string{"export", "web", "--output", "out.json"}, failingWriter{}, io.Discard, func(context.Context, config.Config) (serviceSession, error) { return session, nil }); code != 1 || session.closes != 1 {
		t.Fatal("export output failure ignored or session leaked")
	}
	data, err := os.ReadFile("out.json")
	if err != nil || !json.Valid(data) {
		t.Fatal("stdout failure must leave a complete published archive")
	}
	if code := run(context.Background(), []string{"service", "remove"}, io.Discard, failingWriter{}); code != 1 {
		t.Fatal("diagnostic failure ignored")
	}
}
