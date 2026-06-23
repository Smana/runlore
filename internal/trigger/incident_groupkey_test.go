package trigger

import (
	"strings"
	"testing"
)

func TestParseAlertmanagerGroupKey(t *testing.T) {
	body := `{"groupKey":"{}:{alertname=\"X\"}","alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp1"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp2"}]}`
	incs, err := ParseAlertmanager(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(incs) != 2 {
		t.Fatalf("want 2 incidents, got %d", len(incs))
	}
	for _, inc := range incs {
		if inc.GroupKey != `{}:{alertname="X"}` {
			t.Fatalf("GroupKey not threaded: %q", inc.GroupKey)
		}
	}
}
