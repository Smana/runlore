package gcpfirewall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}

	// First (newest) entry.
	first := got[0]
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

	// Second entry: empty rule reference renders as "?".
	second := got[1]
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
