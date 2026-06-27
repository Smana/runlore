package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/coalesce"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

// newTestServerCoalescing builds a Server whose alertmanager webhook feeds a
// pipeline that enqueues into a Coalescer (MaxBatch=3, so three same-group alerts
// flush synchronously, no sweeper needed). onFlush is called once per flushed batch.
func newTestServerCoalescing(t *testing.T, onFlush func()) *Server {
	t.Helper()
	enq := &spyEnqueuer{}
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		// No severity filter — accept all so the three "warning" alerts pass admission.
	}
	cz := coalesce.New(coalesce.Config{MaxBatch: 3, Debounce: 0}, func(batch []investigate.Request) {
		enq.Enqueue(batch[0])
		onFlush()
	})
	// The coalescer is the pipeline's enqueuer (it implements investigate.Enqueuer).
	return newAlertServer(cfg, cz, nil)
}

func TestWebhookCoalescesGroup(t *testing.T) {
	flushes := 0
	srv := newTestServerCoalescing(t, func() { flushes++ })

	body := `{"groupKey":"gk","alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"1"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"2"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns","severity":"warning"},"fingerprint":"3"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	if flushes != 1 {
		t.Fatalf("3 correlated alerts should coalesce to 1 flush, got %d", flushes)
	}
}
