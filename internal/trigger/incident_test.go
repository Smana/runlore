package trigger

import (
	"strings"
	"testing"
)

func TestParseAlertmanager(t *testing.T) {
	body := `{"alerts":[
	  {"status":"firing","labels":{"alertname":"HarborProbeFailure","severity":"critical","environment":"prod","namespace":"apps","team":"platform"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"abc123"},
	  {"status":"resolved","labels":{"alertname":"Old"},"startsAt":"2026-06-20T01:00:00Z","fingerprint":"def456"}
	]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 2 {
		t.Fatalf("want 2 incidents (firing + resolved), got %d", len(incs))
	}
	got := incs[0]
	if got.AlertName != "HarborProbeFailure" || got.Severity != "critical" ||
		got.Environment != "prod" || got.Namespace != "apps" || got.Fingerprint != "abc123" {
		t.Fatalf("unexpected incident: %+v", got)
	}
	if got.Status != "firing" || incs[1].Status != "resolved" {
		t.Fatalf("statuses = [%q %q], want [firing resolved]", got.Status, incs[1].Status)
	}
	if got.Labels["team"] != "platform" {
		t.Fatal("labels should be preserved")
	}
}
