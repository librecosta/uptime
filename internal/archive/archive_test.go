package archive

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

type historyReader struct {
	services     []uptimestorage.Service
	daily        []uptimestorage.DailyStatus
	samples      []uptimestorage.TodaySampleStatus
	fail         string
	calls        []string
	dailyOptions uptimestorage.QueryDailyOptions
	todayOptions uptimestorage.QueryTodaySamplesOptions
}

var queryFailure = errors.New("query failure")

func (r *historyReader) ListServices(context.Context) ([]uptimestorage.Service, error) {
	r.calls = append(r.calls, "list")
	if r.fail == "list" {
		return nil, queryFailure
	}
	return r.services, nil
}
func (r *historyReader) QueryDaily(_ context.Context, options uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	r.calls = append(r.calls, "daily")
	r.dailyOptions = options
	if r.fail == "daily" {
		return nil, queryFailure
	}
	return r.daily, nil
}
func (r *historyReader) QueryTodaySamples(_ context.Context, options uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	r.calls = append(r.calls, "today")
	r.todayOptions = options
	if r.fail == "today" {
		return nil, queryFailure
	}
	return r.samples, nil
}

func parseTime(t *testing.T, value string) time.Time {
	t.Helper()
	stamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestArchiveV1(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	reader := &historyReader{
		services: []uptimestorage.Service{{ID: "api-v1", Name: "API v1", Description: "Legacy API",
			CreatedAt: parseTime(t, "2026-01-01T08:00:00+08:00"), LastSeenAt: parseTime(t, "2026-10-15T20:30:00+08:00"), SampleInterval: 10 * time.Second}},
		daily: []uptimestorage.DailyStatus{
			{ServiceID: "api-v1", Day: "2026-10-18", UpSlots: 2, ExpectedSlots: 0, Finalized: false},
			{ServiceID: "api-v1", Day: "2026-01-01", UpSlots: 123, ExpectedSlots: 144, Finalized: true},
		},
		samples: []uptimestorage.TodaySampleStatus{{ServiceID: "api-v1", Day: "2026-10-18", UpSlots: 900}},
	}
	result, err := Build(context.Background(), reader, Options{ID: "api-v1", ApplicationVersion: "test-build", Now: parseTime(t, "2026-10-17T18:00:00Z"), Timezone: zone, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "format": "librecosta-uptime-service-archive",
  "version": 1,
  "application_version": "test-build",
  "exported_at": "2026-10-17T18:00:00Z",
  "timezone": "Asia/Shanghai",
  "service": {
    "id": "api-v1",
    "name": "API v1",
    "description": "Legacy API",
    "created_at": "2026-01-01T00:00:00Z",
    "last_seen_at": "2026-10-15T12:30:00Z",
    "sample_interval": "10s"
  },
  "daily": [
    {
      "day": "2026-01-01",
      "up_slots": 123,
      "expected_slots": 144,
      "finalized": true
    },
    {
      "day": "2026-10-18",
      "up_slots": 2,
      "expected_slots": 0,
      "finalized": false
    }
  ],
  "current_day": {
    "day": "2026-10-18",
    "up_slots": 900,
    "expected_slots_so_far": 720
  }
}`
	if string(data) != want {
		t.Fatalf("v1 JSON changed:\n%s", data)
	}
	if !reflect.DeepEqual(reader.calls, []string{"list", "daily", "today"}) ||
		!reflect.DeepEqual(reader.dailyOptions, uptimestorage.QueryDailyOptions{ServiceIDs: []string{"api-v1"}}) ||
		!reflect.DeepEqual(reader.todayOptions, uptimestorage.QueryTodaySamplesOptions{ServiceIDs: []string{"api-v1"}, Day: "2026-10-18"}) {
		t.Fatalf("unexpected query selection/order: %+v", reader)
	}
	if reader.daily[0].Day != "2026-10-18" {
		t.Fatal("mutated query result")
	}
}

func TestArchiveEmptyAndIntervalFallback(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second, 999 * time.Millisecond, time.Second} {
		reader := &historyReader{services: []uptimestorage.Service{{ID: "empty", SampleInterval: interval}}}
		result, err := Build(context.Background(), reader, Options{ID: "empty", ApplicationVersion: "dev", Now: parseTime(t, "2026-10-17T00:00:00Z"), Timezone: time.UTC, Interval: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		wantInterval := "1m0s"
		if interval == time.Second {
			wantInterval = "1s"
		}
		if result.Service.SampleInterval != wantInterval || result.CurrentDay.UpSlots != 0 || result.CurrentDay.ExpectedSlotsSoFar != 0 ||
			!strings.Contains(string(data), `"daily":[]`) || !strings.Contains(string(data), `"description":""`) || strings.Contains(string(data), "created_at") || strings.Contains(string(data), "last_seen_at") {
			t.Fatalf("empty schema/fallback changed: %s", data)
		}
	}
}

func TestArchiveQueryFailures(t *testing.T) {
	for _, failure := range []string{"missing", "canceled", "list", "daily", "today"} {
		t.Run(failure, func(t *testing.T) {
			reader := &historyReader{services: []uptimestorage.Service{{ID: "service"}}, fail: failure}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want, calls := queryFailure, 1
			if failure == "missing" {
				reader.services = nil
				want = ErrServiceNotFound
			}
			if failure == "canceled" {
				cancel()
				want = context.Canceled
				calls = 0
			}
			if failure == "daily" {
				calls = 2
			}
			if failure == "today" {
				calls = 3
			}
			result, err := Build(ctx, reader, Options{ID: "service", Now: time.Now(), Timezone: time.UTC, Interval: time.Second})
			if !errors.Is(err, want) || len(reader.calls) != calls || !reflect.DeepEqual(result, Archive{}) {
				t.Fatalf("partial result or unexpected queries/error: %+v, %v, %v", result, reader.calls, err)
			}
		})
	}
}

func TestExpectedSlots(t *testing.T) {
	// Corresponds to pinned upstream expectedSlotsSoFarSince/slotOf: the
	// partially occupied first slot is floor-aligned, not rounded up.
	for _, tc := range []struct {
		name, zone, now, created string
		interval                 time.Duration
		want                     int
	}{
		{"midnight", "UTC", "2026-10-17T00:00:00Z", "", 10 * time.Second, 0},
		{"first incomplete", "UTC", "2026-10-17T00:00:09Z", "", 10 * time.Second, 0},
		{"boundary", "UTC", "2026-10-17T00:00:10Z", "", 10 * time.Second, 1},
		{"partial first", "UTC", "2026-10-17T00:00:25Z", "2026-10-17T00:00:05Z", 10 * time.Second, 2},
		{"partial later", "UTC", "2026-10-17T00:00:25Z", "2026-10-17T00:00:15Z", 10 * time.Second, 1},
		{"current slot", "UTC", "2026-10-17T00:00:25Z", "2026-10-17T00:00:21Z", 10 * time.Second, 0},
		{"future today", "UTC", "2026-10-17T00:00:25Z", "2026-10-17T00:00:26Z", 10 * time.Second, 0},
		{"future day", "UTC", "2026-10-17T23:00:00Z", "2026-10-18T00:00:00Z", time.Minute, 0},
		{"previous day", "UTC", "2026-10-18T00:00:10Z", "2026-10-17T23:59:55Z", 10 * time.Second, 1},
		{"UTC+8 midnight", "Asia/Shanghai", "2026-10-17T16:00:00Z", "", time.Minute, 0},
		{"UTC+8 partial", "Asia/Shanghai", "2026-10-17T16:00:25Z", "2026-10-18T00:00:15+08:00", 10 * time.Second, 1},
		{"spring gap", "America/New_York", "2026-03-08T03:00:00-04:00", "", time.Minute, 120},
		{"23 hour day", "America/New_York", "2026-03-08T23:59:59-04:00", "", time.Minute, 1379},
		{"fall repeated hour", "America/New_York", "2026-11-01T01:30:00-05:00", "2026-11-01T01:30:00-04:00", time.Minute, 60},
		{"25 hour day", "America/New_York", "2026-11-01T23:59:59-05:00", "", time.Minute, 1499},
		{"DST next midnight", "America/New_York", "2026-11-02T00:00:00-05:00", "2026-11-01T00:00:00-04:00", time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			var created time.Time
			if tc.created != "" {
				created = parseTime(t, tc.created)
			}
			if got := expectedSlots(parseTime(t, tc.now), created, tc.interval, loc); got != tc.want {
				t.Fatalf("slots=%d want=%d", got, tc.want)
			}
		})
	}
	if got := DefaultFilename(parseTime(t, "2026-10-17T21:00:00+08:00")); got != "uptime-service-20261017T130000Z.json" {
		t.Fatal(got)
	}
}
