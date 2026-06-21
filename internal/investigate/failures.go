package investigate

import (
	"context"
	"log/slog"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

// cascadeFailureReasons are GitOps failure reasons that are symptoms of an
// upstream failure, never a root cause: a Kustomization/Application reporting
// "dependency not ready" is failing only because something it depends on is.
// Investigating these floods the knowledge base with duplicate, low-value
// findings (every downstream resource files its own incident) — so we skip them
// and let the trigger fire on the actual failing root resource instead.
var cascadeFailureReasons = map[string]bool{
	"DependencyNotReady": true, // Flux Kustomization/HelmRelease dependsOn cascade
}

// isCascadeFailure reports whether the event is a downstream cascade symptom
// rather than a root-cause failure worth investigating.
func isCascadeFailure(fe providers.FailureEvent) bool {
	return cascadeFailureReasons[fe.Reason]
}

// DrainFailures forwards GitOps FailureEvents into the queue as investigation
// requests. It drops dependency-cascade symptoms (so only root failures are
// investigated) and dedups by workload (a Ready=False resource emits repeated
// events). A nil dedup disables dedup. It returns when src closes or ctx is done.
func DrainFailures(ctx context.Context, src <-chan providers.FailureEvent, q Enqueuer, dedup *trigger.Deduper, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case fe, ok := <-src:
			if !ok {
				return
			}
			if isCascadeFailure(fe) {
				if log != nil {
					log.Debug("skipping gitops cascade failure (not a root cause)",
						"workload", fe.Workload.Namespace+"/"+fe.Workload.Name, "reason", fe.Reason)
				}
				continue
			}
			if dedup != nil && dedup.Seen(fe.Workload.Namespace+"/"+fe.Workload.Name) {
				continue
			}
			q.Enqueue(FromFailureEvent(fe))
		}
	}
}
