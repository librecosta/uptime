package bbolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

// Config selects the database file and the timeout for acquiring its file lock.
type Config struct {
	// Path is required. Unless MustExist is set, missing parents use mode 0750.
	Path string
	// Timeout defaults to five seconds when zero. Negative values are invalid.
	Timeout time.Duration
	// MustExist opens only an existing database and never creates files or parents.
	MustExist bool
}

// Store persists Uptime state. It is safe for concurrent use and must not be
// copied. Use Open to construct a Store; its zero value is not ready for use.
type Store struct {
	db        *bolt.DB
	now       func() time.Time
	closeOnce sync.Once
	closeErr  error
}

var _ uptimestorage.Store = (*Store)(nil)

// Open opens and validates an existing database or initializes a genuinely new
// file. Existing invalid files are never adopted, initialized, or repaired.
// New files use mode 0600; existing file and directory modes are preserved.
func Open(config Config) (*Store, error) {
	if config.Path == "" {
		return nil, fmt.Errorf("bbolt open: path is required")
	}
	if config.Timeout < 0 {
		return nil, fmt.Errorf("bbolt open: timeout must not be negative")
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	if !config.MustExist {
		if err := os.MkdirAll(filepath.Dir(config.Path), 0750); err != nil {
			return nil, fmt.Errorf("bbolt create parent directory: %w", err)
		}
	}

	var created os.FileInfo
	// NoFreelistSync prevents bbolt itself from writing a freelist during Open,
	// before application identity is checked. Normal syncing is restored below.
	db, err := bolt.Open(config.Path, 0600, &bolt.Options{
		Timeout:        config.Timeout,
		NoFreelistSync: true,
		OpenFile: func(path string, flag int, mode os.FileMode) (*os.File, error) {
			var f *os.File
			var err error
			isNew := false
			if config.MustExist {
				// Enforce this on the actual open, not a racy Stat followed by create.
				f, err = os.OpenFile(path, flag&^os.O_CREATE, mode)
			} else {
				f, err = os.OpenFile(path, flag|os.O_EXCL, mode)
				isNew = err == nil
				if errors.Is(err, os.ErrExist) {
					f, err = os.OpenFile(path, flag&^os.O_CREATE, mode)
				}
			}
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			if isNew {
				created = info
			}
			if !info.Mode().IsRegular() || (!isNew && info.Size() == 0) {
				_ = f.Close()
				return nil, fmt.Errorf("existing database must be a non-empty regular file")
			}
			return f, nil
		},
	})
	if err == nil {
		if created != nil {
			db.NoFreelistSync = false
			err = db.Update(initializeSchema)
		}
		if err == nil {
			err = db.View(validateSchema)
		}
		if err == nil {
			db.NoFreelistSync = false
			return &Store{db: db, now: time.Now}, nil
		}
		_ = db.Close()
	}
	// Never remove an existing file or a replacement at the same path.
	if created != nil {
		if current, statErr := os.Stat(config.Path); statErr == nil && os.SameFile(created, current) {
			_ = os.Remove(config.Path)
		}
	}
	return nil, fmt.Errorf("bbolt open %q: %w", config.Path, err)
}

// Name returns the driver name used by Fiber Uptime.
func (s *Store) Name() string { return "bbolt" }

// Close releases the database. Repeated and concurrent calls return the first
// close result. Later persistence operations return errors, not panics.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// Ping checks identity, schema version, and required top-level buckets in a
// read transaction. It does not scan records or guarantee the next write.
func (s *Store) Ping(ctx context.Context) error {
	return s.view(ctx, func(*bolt.Tx) error { return nil })
}

func (s *Store) view(ctx context.Context, fn func(*bolt.Tx) error) error {
	return s.transaction(ctx, false, fn)
}

func (s *Store) update(ctx context.Context, fn func(*bolt.Tx) error) error {
	return s.transaction(ctx, true, fn)
}

func (s *Store) transaction(ctx context.Context, write bool, fn func(*bolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.db == nil {
		return fmt.Errorf("bbolt: store is not open")
	}
	run := func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateSchema(tx); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return ctx.Err()
	}
	var err error
	if write {
		err = s.db.Update(run)
	} else {
		err = s.db.View(run)
	}
	if err != nil {
		return fmt.Errorf("bbolt transaction: %w", err)
	}
	return nil
}
