package bbolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

var testTime = time.Date(2026, 9, 16, 12, 0, 0, 123, time.UTC)

func TestOpenMustExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "uptime.db")
	if s, err := Open(Config{Path: path, MustExist: true}); s != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("MustExist created a parent directory")
	}
	path = filepath.Join(dir, "uptime.db")
	if s, err := Open(Config{Path: path, MustExist: true}); s != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("MustExist created a file")
	}
	for _, invalid := range [][]byte{nil, []byte("unrelated data")} {
		must(t, os.WriteFile(path, invalid, 0600))
		if s, err := Open(Config{Path: path, MustExist: true}); s != nil || err == nil {
			t.Fatal("adopted invalid file")
		}
		data, err := os.ReadFile(path)
		must(t, err)
		if !bytes.Equal(data, invalid) {
			t.Fatal("changed invalid file")
		}
	}
	must(t, os.Remove(path))
	created, err := Open(Config{Path: path})
	must(t, err)
	register(t, created, "existing", 1)
	must(t, created.Close())
	reopened, err := Open(Config{Path: path, MustExist: true})
	must(t, err)
	defer reopened.Close()
	rows, err := reopened.ListServices(context.Background())
	must(t, err)
	if len(rows) != 1 || rows[0].ID != "existing" {
		t.Fatal("existing history changed")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantError(t *testing.T, err error, part string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), part) {
		t.Fatalf("error = %v, want containing %q", err, part)
	}
}
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "uptime.db")})
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	s.now = func() time.Time { return testTime }
	return s
}
func register(t *testing.T, s *Store, id string, instanceID int64) {
	t.Helper()
	must(t, s.UpsertService(context.Background(), uptimestorage.Service{ID: id, SampleInterval: time.Second, LastSeenAt: testTime}))
	must(t, s.UpsertInstance(context.Background(), uptimestorage.Instance{ID: instanceID, ServiceID: id, LastSeenAt: testTime}))
}
func beat(t *testing.T, s *Store, id, day string, instance, slot int64) {
	t.Helper()
	must(t, s.WriteHeartbeat(context.Background(), uptimestorage.Heartbeat{ServiceID: id, InstanceID: instance, Day: day, Slot: slot, SeenAt: testTime}))
}
func readTestInstance(t *testing.T, s *Store, id int64) (uptimestorage.Instance, bool) {
	t.Helper()
	var instance uptimestorage.Instance
	found := false
	must(t, s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("instances")).Bucket(encodeInt64(id))
		if b == nil {
			return nil
		}
		found = true
		var err error
		instance, err = readInstance(b, encodeInt64(id))
		return err
	}))
	return instance, found
}
func hasDay(t *testing.T, s *Store, root, id, day string) bool {
	t.Helper()
	found := false
	must(t, s.db.View(func(tx *bolt.Tx) error {
		b, err := historyDay(tx, root, id, day, false)
		found = b != nil
		return err
	}))
	return found
}

func TestOpenSchemaAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new-parent", "uptime.db")
	s, err := Open(Config{Path: path})
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	if s.Name() != "bbolt" {
		t.Fatal(s.Name())
	}
	must(t, s.Ping(context.Background()))
	must(t, s.db.View(func(tx *bolt.Tx) error {
		for _, name := range topBuckets {
			if tx.Bucket([]byte(name)) == nil {
				t.Fatalf("missing bucket %s", name)
			}
		}
		meta := tx.Bucket([]byte("meta"))
		if string(meta.Get([]byte("format"))) != "deepfurry-uptime-bbolt" {
			t.Fatal("wrong format")
		}
		version, err := decodeUint64(meta.Get([]byte("schema_version")))
		if err != nil || version != 1 {
			t.Fatalf("version = %d, %v", version, err)
		}
		return nil
	}))
	must(t, s.Close())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		must(t, err)
		if info.Mode().Perm()&^0600 != 0 {
			t.Fatalf("file mode = %o", info.Mode().Perm())
		}
		info, err = os.Stat(filepath.Dir(path))
		must(t, err)
		if info.Mode().Perm()&^0750 != 0 {
			t.Fatalf("directory mode = %o", info.Mode().Perm())
		}
		must(t, os.Chmod(path, 0640))
		must(t, os.Chmod(filepath.Dir(path), 0755))
	}
	reopened, err := Open(Config{Path: path})
	must(t, err)
	must(t, reopened.Close())
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		must(t, err)
		if info.Mode().Perm() != 0640 {
			t.Fatal("existing file permissions changed")
		}
		info, err = os.Stat(filepath.Dir(path))
		must(t, err)
		if info.Mode().Perm() != 0755 {
			t.Fatal("existing directory permissions changed")
		}
	}
}

func TestOpenInvalidConfig(t *testing.T) {
	_, err := Open(Config{})
	wantError(t, err, "path")
	_, err = Open(Config{Path: filepath.Join(t.TempDir(), "db"), Timeout: -time.Second})
	wantError(t, err, "timeout")
	_, err = Open(Config{Path: t.TempDir()})
	if err == nil {
		t.Fatal("opened directory")
	}
	parent := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(parent, []byte("file"), 0600))
	_, err = Open(Config{Path: filepath.Join(parent, "db")})
	wantError(t, err, "parent")
}

func TestExistingInvalidDatabaseUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents []byte
	}{
		{"empty", nil}, {"unrelated file", []byte("this is not bbolt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.db")
			must(t, os.WriteFile(path, tc.contents, 0600))
			_, err := Open(Config{Path: path})
			if err == nil {
				t.Fatal("accepted invalid database")
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if !bytes.Equal(after, tc.contents) {
				t.Fatal("existing file changed")
			}
		})
	}
}

func TestInvalidSchemaIsNotRepaired(t *testing.T) {
	fixtures := []struct {
		name, message string
		mutate        func(*bolt.Tx) error
	}{
		{"empty bbolt", "meta", func(tx *bolt.Tx) error {
			for _, name := range topBuckets {
				if err := tx.DeleteBucket([]byte(name)); err != nil {
					return err
				}
			}
			return nil
		}},
		{"missing meta", "meta", func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("meta")) }},
		{"missing format", "format", func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Delete([]byte("format")) }},
		{"wrong format", "format", func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("format"), []byte("other-db")) }},
		{"missing version", "schema_version", func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Delete([]byte("schema_version")) }},
		{"short version", "8 bytes", func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte{1}) }},
		{"old version", "unsupported version 0", func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), encodeUint64(0))
		}},
		{"future version", "unsupported version 2", func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), encodeUint64(2))
		}},
	}
	for _, name := range []string{"services", "instances", "samples", "daily"} {
		fixtures = append(fixtures, struct {
			name, message string
			mutate        func(*bolt.Tx) error
		}{"missing " + name, name, func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(name)) }})
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.db")
			db, err := bolt.Open(path, 0600, &bolt.Options{NoFreelistSync: true})
			must(t, err)
			must(t, db.Update(initializeSchema))
			must(t, db.Update(tc.mutate))
			must(t, db.Close())
			before, err := os.ReadFile(path)
			must(t, err)
			_, err = Open(Config{Path: path})
			wantError(t, err, tc.message)
			after, err := os.ReadFile(path)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid existing database was modified")
			}
		})
	}
}

func TestLifecycleAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	s, err := Open(Config{Path: path})
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	_, err = Open(Config{Path: path, Timeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("second owner acquired database")
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		t.Fatalf("lock error provenance lost: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	must(t, s.Close())
	for name, op := range operations(s) {
		t.Run(name+" after close", func(t *testing.T) {
			if err := op(context.Background()); err == nil {
				t.Fatal("closed operation succeeded")
			}
		})
	}
	reopened, err := Open(Config{Path: path})
	must(t, err)
	must(t, reopened.Close())
}

func operations(s *Store) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"ping":     s.Ping,
		"service":  func(ctx context.Context) error { return s.UpsertService(ctx, uptimestorage.Service{ID: "a"}) },
		"instance": func(ctx context.Context) error { return s.UpsertInstance(ctx, uptimestorage.Instance{ID: 1}) },
		"heartbeat": func(ctx context.Context) error {
			return s.WriteHeartbeat(ctx, uptimestorage.Heartbeat{ServiceID: "a", Day: "2026-09-16"})
		},
		"rollup":  func(ctx context.Context) error { return s.RollupDaily(ctx, uptimestorage.RollupOptions{}) },
		"cleanup": func(ctx context.Context) error { return s.Cleanup(ctx, uptimestorage.CleanupOptions{}) },
		"list":    func(ctx context.Context) error { _, err := s.ListServices(ctx); return err },
		"daily": func(ctx context.Context) error {
			_, err := s.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
			return err
		},
		"today": func(ctx context.Context) error {
			_, err := s.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{})
			return err
		},
		"remove": func(ctx context.Context) error { return s.RemoveService(ctx, "a") },
	}
}

func TestPreCanceledContextAndZeroStore(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, op := range operations(s) {
		t.Run(name, func(t *testing.T) {
			if err := op(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for name, op := range operations(&Store{}) {
		t.Run(name+" zero Store", func(t *testing.T) {
			if err := op(context.Background()); err == nil {
				t.Fatal("zero Store succeeded")
			}
		})
	}
}
