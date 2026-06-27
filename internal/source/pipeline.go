package source

import (
	"context"
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// ResolveFunc records a resolved alert. main supplies a closure over the
// outcome.Ledger (+ metrics), so the pipeline stays decoupled from the ledger's
// concrete episode type. A nil ResolveFunc disables resolved-alert handling.
type ResolveFunc func(fingerprint string, at time.Time)

type Pipeline struct {
	cfg     *config.Config
	enq     investigate.Enqueuer
	resolve ResolveFunc
	dedup   *trigger.Deduper
	log     *slog.Logger
}

func NewPipeline(cfg *config.Config, enq investigate.Enqueuer, resolve ResolveFunc, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, enq: enq, resolve: resolve, log: log,
		dedup: trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()),
	}
}

// Ingest admits each Request per the admission mode and invokes resolve for each
// Resolution. Cascade-suppression and debounce for EnableGated sources are
// applied at the watcher edge (see Task 6) during Phase 1.
func (p *Pipeline) Ingest(ctx context.Context, adm Admission, res DecodeResult) {
	for _, r := range res.Resolved {
		if p.resolve != nil {
			p.resolve(r.Fingerprint, r.At)
		}
	}
	for _, req := range res.Requests {
		if !p.admit(adm, req) {
			continue
		}
		p.enq.Enqueue(req)
	}
}

func (p *Pipeline) admit(adm Admission, r investigate.Request) bool {
	switch adm {
	case MatchGated:
		if !trigger.MatchRequest(p.cfg.Triggers.Incidents, r.Title, r.Severity, r.Environment, r.Workload.Namespace, r.Labels) {
			return false
		}
	case EnableGated:
		if !p.cfg.Triggers.GitOpsFailures.Enabled {
			return false
		}
	default:
		return false
	}
	if p.dedup.Seen(dedupKey(r)) {
		return false
	}
	return true
}

func dedupKey(r investigate.Request) string {
	if r.Fingerprint != "" {
		return r.Fingerprint
	}
	return string(r.Source) + "/" + r.Environment + "/" + r.Workload.Namespace + "/" + r.Workload.Name + "/" + r.Title
}
