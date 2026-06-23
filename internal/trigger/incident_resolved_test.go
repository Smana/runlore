package trigger

import (
	"strings"
	"testing"
)

func TestParseAlertmanagerSurfacesResolved(t *testing.T) {
	body := `{"alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"},
		{"status":"resolved","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"}]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 2 {
		t.Fatalf("want 2 incidents (firing+resolved), got %d", len(incs))
	}
	if incs[0].Status != "firing" || incs[1].Status != "resolved" {
		t.Fatalf("statuses = [%q %q], want [firing resolved]", incs[0].Status, incs[1].Status)
	}
}
