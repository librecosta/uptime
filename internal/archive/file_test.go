package archive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func assertFiles(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("unexpected files: %v", entries)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".uptime-archive-") {
			t.Fatal("temporary file remains")
		}
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.json")
	document := Archive{Format: "librecosta-uptime-service-archive", Version: 1, Service: Service{ID: "example"}, Daily: []Daily{}}
	if err := WriteFile(path, document); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result Archive
	if err := json.Unmarshal(data, &result); err != nil || result.Service.ID != "example" || !strings.HasSuffix(string(data), "\n") || !strings.Contains(string(data), "\n  \"version\": 1") {
		t.Fatalf("invalid file: %s (%v)", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%v", info.Mode())
	}
	if err := WriteFile(path, Archive{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("existing file changed")
	}
	assertFiles(t, dir, 1)
	if err := WriteFile(filepath.Join(dir, "missing", "archive.json"), document); err == nil {
		t.Fatal("missing parent succeeded")
	}
	assertFiles(t, dir, 1)
}

func TestWriteFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dangling := range []bool{false, true} {
		path := filepath.Join(dir, "link.json")
		linkTarget := target
		if dangling {
			linkTarget = filepath.Join(dir, "absent.json")
		}
		if err := os.Symlink(linkTarget, path); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if err := WriteFile(path, Archive{}); !errors.Is(err, os.ErrExist) {
			t.Fatalf("symlink target: %v", err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("symlink replaced")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep" {
		t.Fatal("symlink destination changed")
	}
	assertFiles(t, dir, 1)
}

func TestPublishFailuresCleanUp(t *testing.T) {
	for _, failure := range []string{"write", "sync", "publish"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "archive.json")
			cause := errors.New("injected failure")
			err := publish(path, func(f *os.File) error {
				if _, err := f.WriteString("partial"); err != nil {
					return err
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("target visible before complete publication")
				}
				if failure == "write" {
					return cause
				}
				if failure == "sync" {
					return f.Close()
				}
				return nil
			}, func(source, target string) error {
				if failure == "publish" {
					return cause
				}
				return os.Link(source, target)
			})
			if err == nil {
				t.Fatal("failure reported success")
			}
			if failure != "sync" && !errors.Is(err, cause) {
				t.Fatalf("lost failure: %v", err)
			}
			assertFiles(t, dir, 0)
		})
	}
}

func TestConcurrentPublicationDoesNotReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.json")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			errs <- WriteFile(path, Archive{Version: 1, Service: Service{ID: strings.Repeat("complete", 10000)}})
		})
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful publishers=%d", success)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("incomplete publication: %v", err)
	}
	assertFiles(t, dir, 1)
}
