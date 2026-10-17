// Package archive exports service history through the public Store query contract.
package archive

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

// ErrServiceNotFound means the requested service is not registered in storage.
var ErrServiceNotFound = errors.New("service not found")

// Reader is the read-only portion of the upstream Store used by an export.
type Reader interface {
	ListServices(context.Context) ([]uptimestorage.Service, error)
	QueryDaily(context.Context, uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error)
	QueryTodaySamples(context.Context, uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error)
}

// Options supplies the export identity, clock and normalized sampling settings.
type Options struct {
	ID                 string
	ApplicationVersion string
	Now                time.Time
	Timezone           *time.Location
	Interval           time.Duration
}

// Archive is the frozen v1 JSON envelope. It deliberately contains no config,
// instance metadata or raw sample IDs. Daily and CurrentDay are separate sources.
type Archive struct {
	Format             string     `json:"format"`
	Version            int        `json:"version"`
	ApplicationVersion string     `json:"application_version"`
	ExportedAt         string     `json:"exported_at"`
	Timezone           string     `json:"timezone"`
	Service            Service    `json:"service"`
	Daily              []Daily    `json:"daily"`
	CurrentDay         CurrentDay `json:"current_day"`
}

// Service contains persisted display metadata and UTC timestamps. Zero timestamps
// are omitted; an empty description is retained as an empty string.
type Service struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	CreatedAt      string `json:"created_at,omitempty"`
	LastSeenAt     string `json:"last_seen_at,omitempty"`
	SampleInterval string `json:"sample_interval"`
}

// Daily preserves a stored daily row, including its original counts and finality.
type Daily struct {
	Day           string `json:"day"`
	UpSlots       int    `json:"up_slots"`
	ExpectedSlots int    `json:"expected_slots"`
	Finalized     bool   `json:"finalized"`
}

// CurrentDay combines raw up slots with a theoretical completed-slot count.
// ExpectedSlotsSoFar does not establish that a detached service was monitored.
type CurrentDay struct {
	Day                string `json:"day"`
	UpSlots            int    `json:"up_slots"`
	ExpectedSlotsSoFar int    `json:"expected_slots_so_far"`
}

// Build reads all persisted daily rows for one service and today's raw summary.
// It never rolls up, fills missing dates, or truncates to the UI window. Separate
// queries do not provide a transactionally consistent snapshot on Redis.
func Build(ctx context.Context, reader Reader, options Options) (Archive, error) {
	if err := ctx.Err(); err != nil {
		return Archive{}, err
	}
	services, err := reader.ListServices(ctx)
	if err != nil {
		return Archive{}, err
	}
	index := slices.IndexFunc(services, func(s uptimestorage.Service) bool { return s.ID == options.ID })
	if index < 0 {
		return Archive{}, ErrServiceNotFound
	}
	s := services[index]
	interval := s.SampleInterval
	if interval < time.Second {
		interval = options.Interval
	}
	if options.Timezone == nil || interval < time.Second {
		return Archive{}, errors.New("invalid archive timezone or sample interval")
	}
	day := options.Now.In(options.Timezone).Format(time.DateOnly)
	daily, err := reader.QueryDaily(ctx, uptimestorage.QueryDailyOptions{ServiceIDs: []string{s.ID}})
	if err != nil {
		return Archive{}, err
	}
	samples, err := reader.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{ServiceIDs: []string{s.ID}, Day: day})
	if err != nil {
		return Archive{}, err
	}
	result := Archive{
		Format: "librecosta-uptime-service-archive", Version: 1,
		ApplicationVersion: options.ApplicationVersion, ExportedAt: utc(options.Now), Timezone: options.Timezone.String(),
		Service: Service{ID: s.ID, Name: s.Name, Description: s.Description,
			CreatedAt: utc(s.CreatedAt), LastSeenAt: utc(s.LastSeenAt), SampleInterval: interval.String()},
		Daily:      make([]Daily, 0, len(daily)),
		CurrentDay: CurrentDay{Day: day, ExpectedSlotsSoFar: expectedSlots(options.Now, s.CreatedAt, interval, options.Timezone)},
	}
	for _, row := range daily {
		result.Daily = append(result.Daily, Daily{Day: row.Day, UpSlots: row.UpSlots, ExpectedSlots: row.ExpectedSlots, Finalized: row.Finalized})
	}
	slices.SortFunc(result.Daily, func(a, b Daily) int { return strings.Compare(a.Day, b.Day) })
	for _, row := range samples {
		if row.ServiceID == s.ID && row.Day == day {
			result.CurrentDay.UpSlots = row.UpSlots
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return Archive{}, err
	}
	return result, nil
}

func utc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Match pinned Uptime's expectedSlotsSoFarSince: floor both slot indices and
// exclude the current slot. Calendar boundaries and Sub preserve DST elapsed time.
func expectedSlots(now, createdAt time.Time, interval time.Duration, loc *time.Location) int {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	first := start
	if createdAt.After(first) {
		first = createdAt
	}
	if !first.Before(end) || first.After(now) {
		return 0
	}
	return max(0, int(now.Sub(start)/interval)-int(first.Sub(start)/interval))
}

// DefaultFilename returns a safe UTC filename independent of the service ID.
func DefaultFilename(now time.Time) string {
	return "uptime-service-" + now.UTC().Format("20060102T150405Z") + ".json"
}
