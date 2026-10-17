package service

import (
	"reflect"
	"strings"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

func TestList(t *testing.T) {
	stamp := time.Date(2026, 10, 17, 20, 0, 0, 0, time.FixedZone("+08", 8*3600))
	for _, tc := range []struct {
		name   string
		rows   []uptimestorage.Service
		active []string
		want   []string
	}{
		{"empty", nil, []string{"unpersisted"}, nil},
		{"active", []uptimestorage.Service{{ID: "a", LastSeenAt: stamp}}, []string{"a"}, []string{"a active 2026-10-17T12:00:00Z"}},
		{"detached", []uptimestorage.Service{{ID: "old"}}, nil, []string{"old detached -"}},
		{"mixed", []uptimestorage.Service{{ID: "z"}, {ID: "a", LastSeenAt: stamp}}, []string{"z", "unpersisted"}, []string{"a detached 2026-10-17T12:00:00Z", "z active -"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := List(tc.rows, tc.active)
			lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
			if strings.Join(strings.Fields(lines[0]), " ") != "ID STATUS LAST SEEN (UTC)" || len(lines) != len(tc.want)+1 {
				t.Fatalf("unexpected table: %q", output)
			}
			for i, want := range tc.want {
				if got := strings.Join(strings.Fields(lines[i+1]), " "); got != want {
					t.Fatalf("row=%q want=%q", got, want)
				}
			}
		})
	}
	rows := []uptimestorage.Service{{ID: "z"}, {ID: "a\n\x1b[31m\t\u202e"}}
	before := append([]uptimestorage.Service(nil), rows...)
	output := List(rows, nil)
	if !reflect.DeepEqual(rows, before) || strings.ContainsAny(output, "\x1b\u202e\t") || strings.Count(output, "\n") != 3 || !strings.Contains(output, `\n\x1b[31m\t\u202e`) {
		t.Fatalf("unsafe table or mutated input: %q", output)
	}
	output = List([]uptimestorage.Service{{ID: "invalid\x9b31m\xff"}}, nil)
	if strings.ContainsAny(output, "\x9b\xff") || !strings.Contains(output, `invalid\x9b31m\xff`) {
		t.Fatalf("invalid UTF-8 escaped into terminal: %q", output)
	}
}
