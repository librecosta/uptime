// Package service derives configuration status and guards explicit service removal.
package service

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

// List formats only persisted services, sorted by ID. Active means present in
// the current configuration, not online; detached does not imply no writers.
func List(services []uptimestorage.Service, activeIDs []string) string {
	active := activeSet(activeIDs)
	rows := slices.Clone(services)
	slices.SortFunc(rows, func(a, b uptimestorage.Service) int { return strings.Compare(a.ID, b.ID) })
	var output strings.Builder
	w := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tLAST SEEN (UTC)")
	for _, row := range rows {
		status, lastSeen := "detached", "-"
		if active[row.ID] {
			status = "active"
		}
		if !row.LastSeenAt.IsZero() {
			lastSeen = row.LastSeenAt.UTC().Format(time.RFC3339)
		}
		id := row.ID
		if id == "" || !utf8.ValidString(id) || strings.IndexFunc(id, func(r rune) bool { return !unicode.IsPrint(r) || unicode.IsSpace(r) }) >= 0 {
			id = strconv.QuoteToASCII(id)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", id, status, lastSeen)
	}
	_ = w.Flush() // strings.Builder writes cannot fail.
	return output.String()
}

func activeSet(ids []string) map[string]bool {
	active := make(map[string]bool, len(ids))
	for _, id := range ids {
		active[id] = true
	}
	return active
}
