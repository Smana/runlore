package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
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
	srv := testServerWith(enq)
	srv.SetOutcomeLedger(led)

	body := `{"alerts":[{"status":"resolved","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"fingerprint":"fp1"}]}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body)))

	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("resolved alert must not enqueue an investigation, got %d", len(enq.reqs))
	}
	// the handler's Resolve consumed the seeded open; a second Resolve finds nothing.
	if _, ok, _ := led.Resolve("fp1", time.Unix(2000, 0)); ok {
		t.Fatal("handler should have consumed the open for fp1 (open-index empty after resolve)")
	}
}
