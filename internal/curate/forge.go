// Package curate is the Phase-2 grooming agent: it dedups the KB backlog, gates
// the decision-ready queue on incident resolution, surfaces recurring blind spots
// as knowledge-gap issues, and drives lifecycle/decay. It writes to the forge only
// — it never merges and never touches a human-labelled artifact.
package curate

import (
	"context"

	"github.com/Smana/runlore/internal/providers"
)

// Forge is the forge surface the groomer needs (all read/label/close — never merge).
type Forge interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	Comment(ctx context.Context, number int, body string) error
	ReplaceLabel(ctx context.Context, number int, remove, add string) error
	Close(ctx context.Context, number int) error
	OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error)
}
