package source

import (
	"context"
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/trigger"
)

// ResolveFunc records a resolved alert. main supplies a closure over the
// outcome.Ledger (+ metrics), so the pipeline stays decoupled from the ledger's
// concrete episode type. A nil ResolveFunc disables resolved-alert handling.
type ResolveFunc func(fingerprint string, at time.Time)

// Pipeline admits inbound events from all source adapters, applies the
// configured gate policy (MatchGated or EnableGated), deduplicates within the
// configured window, and forwards survivors to the investigation enqueuer.
type Pipeline struct {
	cfg      *config.Config
	enq      investigate.Enqueuer
	resolve  ResolveFunc
	dedup    *trigger.Deduper
	debounce *incidentDebouncer
	metrics  *telemetry.Metrics // optional; nil-safe ingress counters
	log      *slog.Logger

	// baseCtx bounds the lifetime of background debounce holds, which outlive the
	// per-request HTTP context that delivered the alert (a hold survives seconds to
	// minutes; the webhook handler returns immediately). Defaults to
	// context.Background(); serve binds it to the app/work context via WithContext
	// so holds stop cleanly on shutdown.
	baseCtx context.Context
}

// NewPipeline creates a Pipeline. resolve may be nil to disable resolved-alert
// handling; metrics may be attached later via WithMetrics.
func NewPipeline(cfg *config.Config, enq investigate.Enqueuer, resolve ResolveFunc, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, enq: enq, resolve: resolve, log: log,
		dedup:    trigger.NewDeduper(cfg.Triggers.Incidents.Dedup.Window.Std()),
		debounce: newIncidentDebouncer(cfg.Triggers.Incidents.Debounce.Std(), log),
		baseCtx:  context.Background(),
	}
}

// WithContext binds the lifetime of background debounce holds to ctx (typically
// the app/work context) so they stop cleanly on shutdown instead of running to
// their window end. Chains off NewPipeline; a nil ctx is ignored.
func (p *Pipeline) WithContext(ctx context.Context) *Pipeline {
	if ctx != nil {
		p.baseCtx = ctx
	}
	return p
}

// WithMetrics attaches the OTel metrics instance so admitted alerts (MatchGated)
// increment AlertsReceived and debounced self-resolving alerts increment
// IncidentsDebounced. Chains off NewPipeline; nil m is a no-op.
func (p *Pipeline) WithMetrics(m *telemetry.Metrics) *Pipeline {
	p.metrics = m
	p.debounce.withMetrics(m)
	return p
}

// Ingest admits each Request per the admission mode and invokes resolve for each
// Resolution. Cascade-suppression and debounce for EnableGated sources are
// applied at the watcher edge (see Task 6) during Phase 1.
func (p *Pipeline) Ingest(ctx context.Context, adm Admission, res DecodeResult) {
	for _, r := range res.Resolved {
		// Drop any firing alert still held in the debounce window: it self-resolved
		// before its investigation began, so it never reaches the enqueuer.
		p.debounce.Cancel(r.Fingerprint)
		if p.resolve != nil {
			p.resolve(r.Fingerprint, r.At)
		}
	}
	for _, req := range res.Requests {
		ok, reason := p.admit(adm, req)
		// Per-incident trigger decision log (preserves the legacy handleAlertmanager
		// "incident … investigate=…" line) — alerts only; GitOps failures bypass the pipeline.
		if adm == MatchGated && p.log != nil {
			p.log.Info("incident",
				"alert", req.Title, "severity", req.Severity, "namespace", req.Workload.Namespace,
				"investigate", ok, "reason", reason)
		}
		if !ok {
			continue
		}
		// AlertsReceived counts admitted alerts only (MatchGated): it preserves the
		// legacy server.go ingress counter, which fired after Decide passed and before
		// coalescing. GitOps failures (EnableGated) never incremented it.
		if adm == MatchGated && p.metrics != nil {
			p.metrics.AlertsReceived.Add(ctx, 1)
		}
		// Pre-investigation debounce (opt-in; window 0 = enqueue immediately). Runs
		// AFTER dedup (re-fires already suppressed) and BEFORE the coalescer sink
		// (survivors are still storm-batched): the hold is released early — dropping
		// the incident — if a matching resolved webhook arrives within the window.
		// The hold runs on baseCtx, not the request ctx (the webhook handler returns
		// before the window elapses).
		p.debounce.Hold(p.baseCtx, req, p.enq)
	}
}

// admit reports whether the request starts an investigation, plus a short reason
// (mirroring the legacy trigger.Engine.Decide reasons) for the per-incident log.
func (p *Pipeline) admit(adm Admission, r investigate.Request) (bool, string) {
	switch adm {
	case MatchGated:
		if !trigger.MatchRequest(p.cfg.Triggers.Incidents, r.Title, r.Severity, r.Environment, r.Workload.Namespace, r.Labels) {
			return false, "filtered by trigger policy"
		}
	case EnableGated:
		// Enablement is gated at source build (sources.gitops.enabled); nothing to
		// check here. Fall through to dedup.
	default:
		return false, "unknown admission mode"
	}
	if p.dedup.Seen(dedupKey(r)) {
		return false, "deduplicated (still-firing)"
	}
	return true, "matched trigger policy"
}

func dedupKey(r investigate.Request) string {
	if r.Fingerprint != "" {
		return r.Fingerprint
	}
	return string(r.Source) + "/" + r.Environment + "/" + r.Workload.Namespace + "/" + r.Workload.Name + "/" + r.Title
}
