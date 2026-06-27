package investigate

import (
	"github.com/Smana/runlore/internal/providers"
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

// IsCascadeFailure reports whether the event is a downstream cascade symptom
// rather than a root-cause failure worth investigating. The gitops watcher source
// adapter drops these so only root failures reach the pipeline.
func IsCascadeFailure(fe providers.FailureEvent) bool {
	return cascadeFailureReasons[fe.Reason]
}
