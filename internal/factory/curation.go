// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// draftedNone is Hold.Drafted when the hold is settled and yielded no PR.
const draftedNone = "none"

// Curator writes a finding to the knowledge base: today's path, *curator.Curator.
type Curator interface {
	Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error)
}

// Observer records what a fix did to an entry, and links a trigger to its drafted PR.
type Observer interface {
	Verify(outcome.Verification) error
	Link(triggerKey, url string, at time.Time) error
}

// PullReader reads a merged fix from the forge.
type PullReader interface {
	PullSummary(ctx context.Context, repo string, number int) (providers.PullSummary, error)
}

// DraftLister finds the open entry PRs, to adopt one instead of drafting it again.
type DraftLister interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	PREntry(ctx context.Context, number int) (providers.PREntry, error)
}

var (
	_ Curator  = (*curator.Curator)(nil)
	_ Observer = (*outcome.Ledger)(nil)
)

// Curation replaces the curator when factory.curation.defer is on. It files first, and holds the
// entry of an incident the factory accepted until the fix is verified or the window ends.
type Curation struct {
	Inner      Curator
	Filer      *Notifier
	Ledger     *Ledger
	Outcome    Observer
	Pulls      PullReader
	Drafts     DraftLister
	Resolve    func(context.Context, kbresolve.Target, providers.AppliedResolution) (kbresolve.Result, error)
	FindEntry  func(fingerprint string) (path string, ok bool)
	Promote    func(ctx context.Context, number int) error
	Window     time.Duration
	Repository string
	Now        func() time.Time
	Log        *slog.Logger
}

// Curate files inv with the factory. On a 201 or a 200 it holds the entry and drafts nothing; any
// other answer takes today's path at once, unless a hold already owns the incident's entry. A recall
// is filed and observed, never drafted.
func (c *Curation) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	got, err := c.Filer.File(ctx, inv)
	if err != nil {
		c.log().Warn("filing with the factory failed", "alert", inv.AlertName, "err", redact.Secrets(err.Error()))
	} else {
		c.Filer.logRefusal(ctx, got, inv)
	}
	// A ledger with no path keeps nothing, so nothing it was asked to hold would ever be drafted.
	accepted := err == nil && (got.Status == http.StatusCreated || got.Status == http.StatusOK) && c.Ledger.path != ""
	if inv.Recalled {
		if accepted {
			c.holdRecall(inv)
		}
		return providers.Ref{}, nil
	}
	if h := c.ownedHold(inv.TriggerKey); h != nil && (!accepted || h.Drafted != "") {
		// A refused detection is no generation, and a set Drafted freezes the snapshot.
		return h.ref(), nil
	}
	if !accepted {
		return c.draft(ctx, inv)
	}
	snapshot, err := json.Marshal(inv)
	if err != nil {
		return c.lostHold(ctx, inv, err)
	}
	var ref providers.Ref
	if err := c.Ledger.Upsert(inv.TriggerKey, func(r *Record) { ref = c.hold(r, inv, snapshot) }); err != nil {
		return c.lostHold(ctx, inv, err)
	}
	return ref, nil
}

// hold writes a fresh investigation the factory accepted into r: a new hold, or the newest snapshot
// of one not yet drafted. The fingerprint is frozen at the first hold, since the text it derives
// from is model-written and moves between generations. An observe-only hold had a recall behind it;
// a fresh investigation means the recall did not fire, so the incident now needs its draft.
func (c *Curation) hold(r *Record, inv providers.Investigation, snapshot []byte) providers.Ref {
	switch h := r.Curation; {
	case h == nil || h.ObserveOnly:
		fresh := &Hold{AcceptedAt: c.now(), Investigation: snapshot, Fingerprint: curator.DupFingerprint(inv)}
		if h != nil {
			fresh.Generations = h.Generations
		}
		r.Curation = fresh
	case h.Drafted == "":
		h.Investigation = snapshot
	default: // drafted since Curate looked
		return h.ref()
	}
	return providers.Ref{}
}

// holdRecall writes the observe-only hold of a recall with no hold yet; any other hold stays as it is.
func (c *Curation) holdRecall(inv providers.Investigation) {
	err := c.Ledger.Upsert(inv.TriggerKey, func(r *Record) {
		if r.Curation == nil {
			r.Curation = &Hold{ObserveOnly: true, Entry: inv.RecalledEntry, Drafted: draftedNone, Fingerprint: curator.DupFingerprint(inv)}
		}
	})
	if err != nil {
		c.log().Warn("holding a recall failed; its fix goes unobserved", "alert", inv.AlertName, "err", err)
	}
}

// ownedHold is the hold of key that owns the entry's draft, nil without one.
func (c *Curation) ownedHold(key string) *Hold {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil || r.Curation.ObserveOnly {
		return nil
	}
	return r.Curation
}

// lostHold answers a hold that could not be written: a standing hold still drafts later, and with
// none the entry drafts now, since a lost hold never loses the entry.
func (c *Curation) lostHold(ctx context.Context, inv providers.Investigation, err error) (providers.Ref, error) {
	if h := c.ownedHold(inv.TriggerKey); h != nil {
		c.log().Warn("updating the hold failed; it keeps its older snapshot", "alert", inv.AlertName, "err", err)
		return h.ref(), nil
	}
	c.log().Warn("holding the entry failed; drafting it now", "alert", inv.AlertName, "err", err)
	return c.draft(ctx, inv)
}

// draft is today's path. A nil Inner drafts nothing.
func (c *Curation) draft(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	if c.Inner == nil {
		return providers.Ref{}, nil
	}
	return c.Inner.Curate(ctx, inv)
}

// ref is the link a detection carries: the drafted PR, when there is one.
func (h *Hold) ref() providers.Ref {
	if h.Drafted == draftedNone {
		return providers.Ref{}
	}
	return providers.Ref{URL: h.Drafted}
}

func (c *Curation) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Curation) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
