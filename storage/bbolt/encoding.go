package bbolt

import (
	"encoding/binary"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

func encodeUint64(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

func encodeInt64(n int64) []byte { return encodeUint64(uint64(n)) }
func decodeUint64(b []byte) (uint64, error) {
	if len(b) != 8 {
		return 0, fmt.Errorf("expected 8 bytes, got %d", len(b))
	}
	return binary.BigEndian.Uint64(b), nil
}
func decodeInt64(b []byte) (int64, error) {
	n, err := decodeUint64(b)
	return int64(n), err
}
func decodeInt(b []byte) (int, error) {
	n, err := decodeInt64(b)
	if err != nil {
		return 0, err
	}
	if int64(int(n)) != n {
		return 0, fmt.Errorf("stored integer %d exceeds platform int range", n)
	}
	return int(n), nil
}
func encodeBool(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}
func decodeBool(b []byte) (bool, error) {
	if len(b) != 1 || b[0] > 1 {
		return false, fmt.Errorf("expected boolean byte 0 or 1")
	}
	return b[0] == 1, nil
}

// encodeTime uses Schema v1's signed Unix nanoseconds, with zero reserved for an
// unset time. The round trip detects overflow before it can corrupt a timestamp.
func encodeTime(t time.Time) ([]byte, error) {
	if t.IsZero() {
		return encodeInt64(0), nil
	}
	n := t.UnixNano()
	if !time.Unix(0, n).Equal(t) {
		return nil, fmt.Errorf("timestamp outside UnixNano range")
	}
	return encodeInt64(n), nil
}
func decodeTime(b []byte) (time.Time, error) {
	n, err := decodeInt64(b)
	if err != nil || n == 0 {
		return time.Time{}, err
	}
	return time.Unix(0, n).UTC(), nil
}
func validDay(day string) error {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil || t.Format(time.DateOnly) != day {
		return fmt.Errorf("invalid day %q: expected canonical YYYY-MM-DD", day)
	}
	return nil
}
func optionalDay(day string) error {
	if day == "" {
		return nil
	}
	return validDay(day)
}

func readTime(b *bolt.Bucket, name, path string) (time.Time, error) {
	v, err := field(b, name, path, false)
	if err != nil || v == nil {
		return time.Time{}, err
	}
	t, err := decodeTime(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s/%s: %w", path, name, err)
	}
	return t, nil
}
func readString(b *bolt.Bucket, name, path string) (string, error) {
	v, err := field(b, name, path, false)
	return string(v), err
}
func readInt(b *bolt.Bucket, name, path string, required bool) (int, error) {
	v, err := field(b, name, path, required)
	if err != nil || v == nil {
		return 0, err
	}
	n, err := decodeInt(v)
	if err != nil {
		return 0, fmt.Errorf("%s/%s: %w", path, name, err)
	}
	return n, nil
}
func readCount(b *bolt.Bucket, name, path string) (int, error) {
	n, err := readInt(b, name, path, true)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("%s/%s: negative count", path, name)
	}
	return n, nil
}
func putTime(b *bolt.Bucket, name, path string, t time.Time) error {
	v, err := encodeTime(t)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", path, name, err)
	}
	return b.Put([]byte(name), v)
}
func advanceTime(b *bolt.Bucket, name, path string, incoming time.Time) error {
	current, err := readTime(b, name, path)
	if err != nil {
		return err
	}
	if incoming.IsZero() {
		return nil
	}
	if current.IsZero() || incoming.After(current) {
		return putTime(b, name, path, incoming)
	}
	return nil
}
