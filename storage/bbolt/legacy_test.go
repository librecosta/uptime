package bbolt

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

func TestOpenOriginalSchemaV1History(t *testing.T) {
	// Freeze the original representation independently of current schema/encoding
	// helpers so a product rename cannot silently invalidate existing databases.
	path := filepath.Join(t.TempDir(), "original.db")
	db, err := bolt.Open(path, 0600, nil)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	u64 := func(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
	must(t, db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "services", "instances", "samples", "daily"} {
			_, err := tx.CreateBucket([]byte(name))
			must(t, err)
		}
		meta := tx.Bucket([]byte("meta"))
		must(t, meta.Put([]byte("format"), []byte("deepfurry-uptime-bbolt")))
		must(t, meta.Put([]byte("schema_version"), u64(1)))
		service, err := tx.Bucket([]byte("services")).CreateBucket([]byte("legacy"))
		must(t, err)
		must(t, service.Put([]byte("name"), []byte("Original service")))
		must(t, service.Put([]byte("sample_interval"), u64(uint64(time.Second))))
		must(t, service.Put([]byte("created_at"), u64(uint64(testTime.UnixNano()))))
		must(t, service.Put([]byte("last_seen_at"), u64(uint64(testTime.UnixNano()))))
		for _, root := range []string{"samples", "daily"} {
			history, err := tx.Bucket([]byte(root)).CreateBucket([]byte("legacy"))
			must(t, err)
			day, err := history.CreateBucket([]byte("2026-09-16"))
			must(t, err)
			must(t, day.Put([]byte("up_slots"), u64(1)))
			if root == "samples" {
				slots, err := day.CreateBucket([]byte("slots"))
				must(t, err)
				must(t, slots.Put(u64(1), []byte{1}))
			} else {
				must(t, day.Put([]byte("expected_slots"), u64(2)))
				must(t, day.Put([]byte("finalized"), []byte{1}))
			}
		}
		return nil
	}))
	must(t, db.Close())

	store, err := Open(Config{Path: path})
	must(t, err)
	defer store.Close()
	ctx := context.Background()
	must(t, store.Ping(ctx))
	services, err := store.ListServices(ctx)
	must(t, err)
	if len(services) != 1 || services[0].ID != "legacy" || services[0].Name != "Original service" || !services[0].CreatedAt.Equal(testTime) {
		t.Fatal("original service metadata changed")
	}
	samples, err := store.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{Day: "2026-09-16"})
	must(t, err)
	if len(samples) != 1 || samples[0].UpSlots != 1 {
		t.Fatal("original sample history changed")
	}
	daily, err := store.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
	must(t, err)
	if len(daily) != 1 || daily[0].UpSlots != 1 || daily[0].ExpectedSlots != 2 || !daily[0].Finalized {
		t.Fatal("original daily history changed")
	}
}
