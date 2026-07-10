// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/outcome"
	"gopkg.in/yaml.v3"
)

func TestHandleAlertmanagerResolvedRoutesToLedger(t *testing.T) {
	led, err := outcome.New(filepath.Join(t.TempDir(), "o.jsonl"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if err := led.Open(outcome.Event{Fingerprint: "fp1", Kind: "recall", Entry: "e.md", At: time.Unix(1000, 0)}); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	enq := &spyEnqueuer{}
	cfg := &config.Config{}
	cfg.Sources = map[string]yaml.Node{"alertmanager": {}}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Match: config.IncidentMatch{Severity: []string{"critical"}},
	}
	// resolve mirrors main's pipeline resolve callback: a resolved alert folds back
	// into the outcome ledger. We also capture the fingerprint to assert routing.
	var resolved []string
	resolve := func(fp string, at time.Time) {
		resolved = append(resolved, fp)
		if _, _, rerr := led.Resolve(fp, at); rerr != nil {
			t.Errorf("ledger resolve: %v", rerr)
		}
	}
	srv := newAlertServer(cfg, enq, resolve)

	body := `{"alerts":[{"status":"resolved","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"fingerprint":"fp1"}]}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body)))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("resolved alert must not enqueue an investigation, got %d", len(enq.reqs))
	}
	if len(resolved) != 1 || resolved[0] != "fp1" {
		t.Fatalf("resolved alert must route to resolve(fp1), got %+v", resolved)
	}
	// the pipeline's resolve consumed the seeded open; a second Resolve finds nothing.
	if _, ok, _ := led.Resolve("fp1", time.Unix(2000, 0)); ok {
		t.Fatal("resolve should have consumed the open for fp1 (open-index empty after resolve)")
	}
}
