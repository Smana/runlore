// SPDX-License-Identifier: Apache-2.0

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

// EntryReader reads the entry a PR carries, as it stands at the PR's head, for the threat check. An
// error says nothing about the text, so a caller treats it as a hit. *github.Client satisfies it, and
// so does Guard.
type EntryReader interface {
	PREntryText(ctx context.Context, number int) ([]byte, error)
}

// QueueForge is what the Queue pass needs: the groomer's forge, and the entry it screens.
type QueueForge interface {
	Forge
	EntryReader
}
