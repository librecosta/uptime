package archive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// WriteFile serializes before creating a file, then publishes complete JSON
// without replacing any existing file or symlink. Filesystems without hard-link
// support fail safely. Parent directories must already exist.
// Visibility relies on the filesystem's link semantics, not a portable rename
// guarantee. Temporary-name cleanup can fail after a complete archive is published.
func WriteFile(path string, archive Archive) error {
	data, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return publish(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}, os.Link)
}

// The callbacks allow failure tests against real temporary files without a
// filesystem abstraction. Only the owned temporary name is ever removed.
func publish(path string, write func(*os.File) error, link func(string, string) error) (result error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".uptime-archive-*")
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, f.Close())
		}
		result = errors.Join(result, os.Remove(f.Name()))
	}()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	closed = true
	if err := f.Close(); err != nil {
		return err
	}
	// Linking the synced sibling file refuses an existing destination. A plain
	// rename could overwrite it; writing directly would expose partial JSON.
	return link(f.Name(), path)
}
