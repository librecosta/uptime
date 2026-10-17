package bbolt

import (
	"context"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// format is the historical persistence identity, retained across product renames
// so existing databases remain readable. It is not a display brand.
const format = "deepfurry-uptime-bbolt"
const schemaVersion uint64 = 1

var topBuckets = [...]string{"meta", "services", "instances", "samples", "daily"}

func initializeSchema(tx *bolt.Tx) error {
	if key, _ := tx.Cursor().First(); key != nil {
		return fmt.Errorf("new database contains unexpected data")
	}
	for _, name := range topBuckets {
		if _, err := tx.CreateBucket([]byte(name)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	meta := tx.Bucket([]byte("meta"))
	if err := meta.Put([]byte("format"), []byte(format)); err != nil {
		return err
	}
	return meta.Put([]byte("schema_version"), encodeUint64(schemaVersion))
}

// validateSchema checks identity and top-level structure, not every stored record.
// Record readers validate accessed fields; unsupported versions are never repaired
// or migrated implicitly.
func validateSchema(tx *bolt.Tx) error {
	for _, name := range topBuckets {
		if tx.Bucket([]byte(name)) == nil {
			return fmt.Errorf("schema: missing required bucket %q", name)
		}
	}
	meta := tx.Bucket([]byte("meta"))
	marker, err := field(meta, "format", "meta", true)
	if err != nil {
		return err
	}
	if string(marker) != format {
		return fmt.Errorf("meta/format: wrong or unsupported database format %q", marker)
	}
	data, err := field(meta, "schema_version", "meta", true)
	if err != nil {
		return err
	}
	version, err := decodeUint64(data)
	if err != nil {
		return fmt.Errorf("meta/schema_version: %w", err)
	}
	if version != schemaVersion {
		return fmt.Errorf("meta/schema_version: unsupported version %d (supported: %d)", version, schemaVersion)
	}
	return nil
}

func field(b *bolt.Bucket, name, path string, required bool) ([]byte, error) {
	if b.Bucket([]byte(name)) != nil {
		return nil, fmt.Errorf("%s/%s: expected value, found bucket", path, name)
	}
	value := b.Get([]byte(name))
	if required && value == nil {
		return nil, fmt.Errorf("%s/%s: missing required value", path, name)
	}
	return value, nil
}

func child(parent *bolt.Bucket, key []byte, path string, create bool) (*bolt.Bucket, error) {
	b := parent.Bucket(key)
	if b != nil {
		return b, nil
	}
	if parent.Get(key) != nil {
		return nil, fmt.Errorf("%s: expected bucket, found value", path)
	}
	if !create {
		return nil, nil
	}
	b, err := parent.CreateBucket(key)
	if err != nil {
		return nil, fmt.Errorf("%s: create bucket: %w", path, err)
	}
	return b, nil
}

func eachBucket(ctx context.Context, b *bolt.Bucket, path string, fn func([]byte, *bolt.Bucket) error) error {
	return b.ForEach(func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if v != nil {
			return fmt.Errorf("%s/%q: expected bucket, found value", path, k)
		}
		return fn(k, b.Bucket(k))
	})
}
