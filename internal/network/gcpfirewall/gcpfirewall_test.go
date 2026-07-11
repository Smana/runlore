// SPDX-License-Identifier: Apache-2.0

package gcpfirewall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// rawPayload builds a googleapi.RawMessage for a firewall jsonPayload.
func rawPayload(t *testing.T, p fwPayload) googleapi.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return googleapi.RawMessage(b)
}

func TestDrops(t *testing.T) {
	ts1 := "2026-06-24T10:00:00Z"
	ts2 := "2026-06-24T09:59:00Z"

	var p1 fwPayload
	p1.Disposition = "DENIED"
	p1.Connection.SrcIP = "10.0.0.5"
	p1.Connection.SrcPort = 54321
	p1.Connection.DestIP = "10.0.1.20"
	p1.Connection.DestPort = 443
	p1.Connection.Protocol = 6
	p1.RuleDetails.Reference = "network:default/firewall:deny-egress"

	var p2 fwPayload
	p2.Disposition = "DENIED"
	p2.Connection.SrcIP = "10.0.0.6"
	p2.Connection.SrcPort = 12345
	p2.Connection.DestIP = "10.0.2.30"
	p2.Connection.DestPort = 5432
	p2.Connection.Protocol = 6
	// No rule reference -> should render as "?".

	resp := logging.ListLogEntriesResponse{
		Entries: []*logging.LogEntry{
			{Timestamp: ts1, JsonPayload: rawPayload(t, p1)},
			{Timestamp: ts2, JsonPayload: rawPayload(t, p2)},
		},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	// Respond to ANY request with the canned entries.list response; the test
	// makes a single call so matching the exact path is unnecessary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ctx := context.Background()
	c, err := New(ctx, "my-proj",
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := c.Drops(ctx, providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}

	// got[0] is the scoping note; got[1..2] are the DENIED flows.
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 (1 note + 2 flows)", len(got))
	}

	// First (newest) DENIED entry is at index 1 (after the scoping note).
	first := got[1]
	wantMsg := "10.0.0.5:54321 -> 10.0.1.20:443 DENIED (network:default/firewall:deny-egress)"
	if first.Message != wantMsg {
		t.Errorf("first message = %q, want %q", first.Message, wantMsg)
	}

	wantFields := map[string]string{
		"disposition": "DENIED",
		"source":      "10.0.0.5",
		"destination": "10.0.1.20",
		"srcport":     "54321",
		"destport":    "443",
		"protocol":    "6",
		"rule":        "network:default/firewall:deny-egress",
	}
	for k, want := range wantFields {
		if first.Fields[k] != want {
			t.Errorf("first field %q = %q, want %q", k, first.Fields[k], want)
		}
	}

	wantTime, _ := time.Parse(time.RFC3339, ts1)
	if !first.Time.Equal(wantTime) {
		t.Errorf("first time = %v, want %v", first.Time, wantTime)
	}

	// Second DENIED entry (index 2): empty rule reference renders as "?".
	second := got[2]
	wantMsg2 := "10.0.0.6:12345 -> 10.0.2.30:5432 DENIED (?)"
	if second.Message != wantMsg2 {
		t.Errorf("second message = %q, want %q", second.Message, wantMsg2)
	}
	if second.Fields["rule"] != "?" {
		t.Errorf("second rule field = %q, want %q", second.Fields["rule"], "?")
	}
	if second.Fields["disposition"] != "DENIED" {
		t.Errorf("second disposition = %q, want DENIED", second.Fields["disposition"])
	}
}

func TestNewRequiresProject(t *testing.T) {
	if _, err := New(context.Background(), ""); err == nil {
		t.Fatal("New with empty project: want error, got nil")
	}
}

// TestDropsScopingNoteFirst asserts that:
//   - The scoping note is always the first entry (index 0) in the result.
//   - The note message contains "NOT scoped to".
//   - When the Selector carries a namespace and/or name, they appear in the note.
func TestDropsScopingNoteFirst(t *testing.T) {
	// Minimal server that returns one DENIED entry for any request.
	var p fwPayload
	p.Disposition = "DENIED"
	p.Connection.SrcIP = "10.0.0.1"
	p.Connection.SrcPort = 1234
	p.Connection.DestIP = "10.0.1.1"
	p.Connection.DestPort = 443
	p.Connection.Protocol = 6
	oneEntry := logging.ListLogEntriesResponse{
		Entries: []*logging.LogEntry{
			{Timestamp: "2026-06-24T10:00:00Z", JsonPayload: rawPayload(t, p)},
		},
	}
	body, _ := json.Marshal(oneEntry)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tests := []struct {
		name      string
		sel       providers.Selector
		wantScope string // substring expected in the note message
	}{
		{
			name:      "empty selector shows placeholder",
			sel:       providers.Selector{},
			wantScope: "<namespace>/<pod>",
		},
		{
			name:      "namespace only",
			sel:       providers.Selector{Namespace: "production"},
			wantScope: "production/<pod>",
		},
		{
			name:      "namespace and name",
			sel:       providers.Selector{Namespace: "production", Name: "api-server"},
			wantScope: "production/api-server",
		},
		{
			name:      "name only",
			sel:       providers.Selector{Name: "api-server"},
			wantScope: "<namespace>/api-server",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, err := New(ctx, "my-proj",
				option.WithHTTPClient(srv.Client()),
				option.WithEndpoint(srv.URL),
				option.WithoutAuthentication(),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := c.Drops(ctx, tc.sel, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("Drops: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("Drops returned empty result, want at least the scoping note")
			}
			note := got[0]
			if !strings.Contains(note.Message, "NOT scoped to") {
				t.Errorf("note missing 'NOT scoped to': %q", note.Message)
			}
			if !strings.Contains(note.Message, tc.wantScope) {
				t.Errorf("note does not contain selector %q: %q", tc.wantScope, note.Message)
			}
			// The note must carry no Time or Fields so it cannot be mistaken for a
			// real flow record.
			if !note.Time.IsZero() {
				t.Errorf("note Time = %v, want zero", note.Time)
			}
			if len(note.Fields) != 0 {
				t.Errorf("note Fields = %v, want empty", note.Fields)
			}
		})
	}
}

// TestDropsScopingNoteEmptyResult asserts the scoping note is present even when
// the Cloud Logging query returns no entries (empty window / no DENIEDs).
func TestDropsScopingNoteEmptyResult(t *testing.T) {
	emptyResp := logging.ListLogEntriesResponse{}
	body, _ := json.Marshal(emptyResp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ctx := context.Background()
	c, err := New(ctx, "my-proj",
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sel := providers.Selector{Namespace: "staging", Name: "worker"}
	got, err := c.Drops(ctx, sel, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1 (scoping note only)", len(got))
	}
	if !strings.Contains(got[0].Message, "staging/worker") {
		t.Errorf("note does not contain selector 'staging/worker': %q", got[0].Message)
	}
}

// deniedEntry builds a DENIED firewall log entry from src/dest octet seeds.
func deniedEntry(t *testing.T, ts, srcIP, destIP string) *logging.LogEntry {
	var p fwPayload
	p.Disposition = "DENIED"
	p.Connection.SrcIP = srcIP
	p.Connection.SrcPort = 1234
	p.Connection.DestIP = destIP
	p.Connection.DestPort = 443
	p.Connection.Protocol = 6
	return &logging.LogEntry{Timestamp: ts, JsonPayload: rawPayload(t, p)}
}

func TestDropsPagination(t *testing.T) {
	// Page 1 carries a nextPageToken; page 2 (served only when pageToken is sent)
	// has no token. Entries differ so we can prove both pages were read.
	page1 := logging.ListLogEntriesResponse{
		Entries:       []*logging.LogEntry{deniedEntry(t, "2026-06-24T10:00:00Z", "10.0.0.1", "10.0.1.1"), deniedEntry(t, "2026-06-24T09:59:00Z", "10.0.0.2", "10.0.1.2")},
		NextPageToken: "tok-2",
	}
	page2 := logging.ListLogEntriesResponse{
		Entries: []*logging.LogEntry{deniedEntry(t, "2026-06-24T09:58:00Z", "10.0.0.3", "10.0.1.3"), deniedEntry(t, "2026-06-24T09:57:00Z", "10.0.0.4", "10.0.1.4")},
	}
	body1, _ := json.Marshal(page1)
	body2, _ := json.Marshal(page2)

	tests := []struct {
		name          string
		maxEvents     int64
		wantDrops     int  // real (non-sentinel) drop lines
		wantTruncated bool // a sentinel line appended
		wantTokenSeen bool // page 2 was requested (pageToken sent)
	}{
		{name: "under cap — both pages, no sentinel", maxEvents: 25, wantDrops: 4, wantTruncated: false, wantTokenSeen: true},
		{name: "over cap — capped + sentinel", maxEvents: 2, wantDrops: 2, wantTruncated: true, wantTokenSeen: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sawToken string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req logging.ListLogEntriesRequest
				if r.Body != nil {
					_ = json.NewDecoder(r.Body).Decode(&req)
				}
				w.Header().Set("Content-Type", "application/json")
				if req.PageToken == "tok-2" {
					sawToken = req.PageToken
					_, _ = w.Write(body2)
					return
				}
				_, _ = w.Write(body1)
			}))
			defer srv.Close()

			ctx := context.Background()
			c, err := New(ctx, "my-proj",
				option.WithHTTPClient(srv.Client()),
				option.WithEndpoint(srv.URL),
				option.WithoutAuthentication(),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			c.maxEvents = tc.maxEvents

			got, err := c.Drops(ctx, providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("Drops: %v", err)
			}

			truncated := 0
			drops := 0
			for _, l := range got {
				switch {
				case strings.Contains(l.Message, "results truncated at"):
					truncated++
				case strings.Contains(l.Message, "NOT scoped to"):
					// scoping note — not a real flow line
				default:
					drops++
				}
			}
			if drops != tc.wantDrops {
				t.Fatalf("drop lines = %d, want %d (total %d)", drops, tc.wantDrops, len(got))
			}
			if (truncated == 1) != tc.wantTruncated {
				t.Fatalf("sentinel present = %v, want %v", truncated == 1, tc.wantTruncated)
			}
			if tc.wantTruncated && got[len(got)-1].Message == "" {
				t.Fatalf("sentinel must be the last line")
			}
			if (sawToken == "tok-2") != tc.wantTokenSeen {
				t.Fatalf("page-2 token seen = %v, want %v", sawToken == "tok-2", tc.wantTokenSeen)
			}
		})
	}
}
