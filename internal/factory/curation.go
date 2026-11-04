// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// draftedNone is Hold.Drafted when the hold is settled and yielded no PR.
const draftedNone = "none"

// maxPastWindowFailures is how many times a step may fail once the window has passed before its failure
// is terminal: about 30 minutes of RunLore running, at one tick a minute. RunLore often runs only in
// sessions, so a window can pass while it is off, and one blip on the next start must not lose an entry.
const maxPastWindowFailures = 30

// maxSnapshotBytes is the largest investigation a hold keeps, well inside the ledger's line limit.
const maxSnapshotBytes = 1 << 20

// Curator writes a finding to the knowledge base: today's path, *curator.Curator. MeetsBar says
// whether it would draft inv at all.
type Curator interface {
	Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error)
	MeetsBar(inv providers.Investigation) bool
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

// DraftLister finds the open entry PRs, to adopt one instead of drafting it again, and labels a PR
// RunLore opened whose best-effort label failed.
type DraftLister interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	PREntry(ctx context.Context, number int) (providers.PREntry, error)
	ReplaceLabel(ctx context.Context, number int, remove, add string) error
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
	// DryRun is true when Resolve's forge writes nothing, so no generation may be marked resolved.
	DryRun bool
	Now    func() time.Time
	Log    *slog.Logger

	once sync.Mutex // a leadership change can start a tick while the old one ends
}

// Curate files inv with the factory, unless the hold's current generation is undecided: a 201 would
// reset the record under a fix that is still running. On a 201, or a 200 with no hold yet, it holds the
// entry and drafts nothing; any other answer takes today's path at once, unless a hold already owns the
// incident's entry. A 200 with a hold standing is no new generation and touches nothing. A 201 on a
// hold with no live artifact starts it again. A recall is filed and observed, never drafted.
func (c *Curation) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	if r, ok := c.Ledger.Get(inv.TriggerKey); ok && c.undecided(ctx, r) {
		c.log().Info("a generation is undecided; filing nothing for this re-detection", "alert", inv.AlertName, "key", inv.TriggerKey)
		if inv.Recalled {
			return providers.Ref{}, nil
		}
		return r.Curation.ref(), nil
	}
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
	h := c.standingHold(inv.TriggerKey)
	owned := h != nil && !h.ObserveOnly
	restart := owned && accepted && got.Status == http.StatusCreated && c.noLiveArtifact(ctx, h)
	switch {
	case owned && !restart && (!accepted || h.Drafted != ""):
		// A refused detection is no generation, and a set Drafted freezes the snapshot.
		return h.ref(), nil
	case h != nil && accepted && got.Status == http.StatusOK:
		return h.ref(), nil
	case !accepted:
		return c.draft(ctx, inv)
	}
	snapshot, err := json.Marshal(inv)
	if err == nil && len(snapshot) > maxSnapshotBytes {
		err = fmt.Errorf("the investigation is %d bytes, past the %d a hold keeps", len(snapshot), maxSnapshotBytes)
	}
	if err != nil {
		return c.lostHold(ctx, inv, err)
	}
	var ref providers.Ref
	if err := c.Ledger.Upsert(inv.TriggerKey, func(r *Record) { ref = c.hold(r, inv, snapshot, restart) }); err != nil {
		return c.lostHold(ctx, inv, err)
	}
	return ref, nil
}

// hold writes a fresh investigation the factory accepted into r: a new hold, or the newest snapshot
// of one not yet drafted. The fingerprint is frozen at the first hold, since the text it derives
// from is model-written and moves between generations, and it is the fingerprint of the PR today's
// path already opened, if any, so the draft adopts that PR. An observe-only hold had a recall behind
// it; a fresh investigation means the recall did not fire, so the incident now needs its draft. A
// newest snapshot that fails the bar does not replace the older one: the draft would drop the entry
// for good, and the verified fix with it. A hold starting over, on a recall or with restart, keeps only
// its failed generations: a held one would release a draft at once, naming an old fix as Applied. A
// restart's failed fixes are credited again, on the entry that will now exist.
func (c *Curation) hold(r *Record, inv providers.Investigation, snapshot []byte, restart bool) providers.Ref {
	switch h := r.Curation; {
	case h == nil || h.ObserveOnly:
		fresh := &Hold{AcceptedAt: c.now(), Investigation: snapshot, Fingerprint: cmp.Or(r.DraftFingerprint, curator.DupFingerprint(inv))}
		if h != nil {
			fresh.Generations = failedGenerations(h.Generations)
		}
		r.Curation = fresh
	case restart:
		gens := failedGenerations(h.Generations)
		for i := range gens {
			gens[i].Observed, gens[i].Resolved = "", ""
		}
		r.Curation = &Hold{AcceptedAt: c.now(), Investigation: snapshot, Fingerprint: h.Fingerprint, Generations: gens}
	case h.Drafted == "":
		if c.meetsBar(inv) {
			h.Investigation = snapshot
		}
	default: // drafted since Curate looked
		return h.ref()
	}
	return providers.Ref{}
}

// undecided reports whether r's hold should keep the next re-detection from filing. A bound fix is
// undecided until a verdict, a refusal, a revert or a regression, until its room ends without a merge,
// or, while it is unmerged, until the window, so a fix that never ends and never merges cannot block
// filing for good. A merged fix keeps its soak past the window: the verifier always reaches a verdict or
// an outcome.
// A hold not drafted, with no fix yet, is one stub waiting for its human until the window: a new triager
// and stub each repeat buy nothing. So is a drafted hold whose PR is open (or unreadable) with no fix
// bound, at any time: it waits until someone acts on it. A merge is that act, so a merged entry files
// again; so does a PR closed unmerged, and a bound, decided fix. An observe-only hold has nothing of its own to protect.
func (c *Curation) undecided(ctx context.Context, r Record) bool {
	h := r.Curation
	switch {
	case h == nil || h.ObserveOnly:
		return false
	case r.FixRoom == "":
		if h.Drafted == "" {
			return !c.windowPassed(h)
		}
		return h.Drafted != draftedNone && c.prOpen(ctx, h)
	}
	return r.Verdict == "" && r.Outcome == "" && !r.Reverted && r.Monitor != "regression" && !r.fixEndedUnmerged() && (!r.MergedAt.IsZero() || !c.windowPassed(h))
}

// noLiveArtifact reports whether h settled with nothing to its name: no PR and no entry, or a PR the
// forge says was closed unmerged. A PR that cannot be read is taken as live.
func (c *Curation) noLiveArtifact(ctx context.Context, h *Hold) bool {
	if h.Drafted == draftedNone {
		return h.Entry == ""
	}
	n, ok := prNumber(h.Drafted)
	if !ok || c.Drafts == nil {
		return false
	}
	pe, err := c.Drafts.PREntry(ctx, n)
	return err == nil && pe.State == "closed"
}

// prOpen reports whether the PR h drafted is open, or cannot be read: a fetch failure must not file.
func (c *Curation) prOpen(ctx context.Context, h *Hold) bool {
	n, ok := prNumber(h.Drafted)
	if !ok || c.Drafts == nil {
		return true
	}
	pe, err := c.Drafts.PREntry(ctx, n)
	return err != nil || pe.State == "open"
}

// failedGenerations is the failed ones of gens, in order.
func failedGenerations(gens []Generation) []Generation {
	var out []Generation
	for _, g := range gens {
		if g.Outcome() == "failed" {
			out = append(out, g)
		}
	}
	return out
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

// standingHold is the hold of key, nil without one.
func (c *Curation) standingHold(key string) *Hold {
	r, ok := c.Ledger.Get(key)
	if !ok {
		return nil
	}
	return r.Curation
}

// lostHold answers a hold that could not be written: a standing hold still drafts later, and with
// none the entry drafts now, since a lost hold never loses the entry.
func (c *Curation) lostHold(ctx context.Context, inv providers.Investigation, err error) (providers.Ref, error) {
	if h := c.standingHold(inv.TriggerKey); h != nil && !h.ObserveOnly {
		c.log().Warn("updating the hold failed; it keeps its older snapshot", "alert", inv.AlertName, "err", err)
		return h.ref(), nil
	}
	c.log().Warn("holding the entry failed; drafting it now", "alert", inv.AlertName, "err", err)
	return c.draft(ctx, inv)
}

// draft is today's path. A nil Inner drafts nothing. A PR it opens leaves its fingerprint on the
// record, so a hold started later adopts it instead of opening a second one.
func (c *Curation) draft(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	if c.Inner == nil {
		return providers.Ref{}, nil
	}
	ref, err := c.Inner.Curate(ctx, inv)
	if err != nil || ref.URL == "" || inv.TriggerKey == "" {
		return ref, err
	}
	werr := c.Ledger.Upsert(inv.TriggerKey, func(r *Record) {
		r.DraftFingerprint = cmp.Or(r.DraftFingerprint, curator.DupFingerprint(inv))
	})
	if werr != nil {
		c.log().Warn("recording the draft's fingerprint failed; a later hold may open a second PR", "alert", inv.AlertName, "err", werr)
	}
	return ref, nil
}

// meetsBar reports whether Inner would draft inv. A nil Inner drafts nothing, so no snapshot is worse
// than another.
func (c *Curation) meetsBar(inv providers.Investigation) bool {
	return c.Inner == nil || c.Inner.MeetsBar(inv)
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

// Run calls Once every interval until ctx ends.
func (c *Curation) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Once(ctx)
		}
	}
}

// Once brings every held incident forward: it drafts or adopts its entry, links the trigger to it,
// observes each generation's outcome, records it in the entry, and promotes the PR. Each step writes
// what it did before the next one reads it, and a crash replays safely: the outcome ledger folds per
// room, and Resolve is idempotent. Anything that fails retries on the next tick; past the hold's window
// each step has a bounded number of attempts (see retries), since holds are never removed and permanent
// errors cannot be listed.
func (c *Curation) Once(ctx context.Context) {
	c.once.Lock()
	defer c.once.Unlock()
	for _, r := range c.Ledger.Held() {
		if ctx.Err() != nil {
			return
		}
		key := r.TriggerKey
		if !c.settle(ctx, key) {
			continue
		}
		c.link(ctx, key)
		c.observe(ctx, key)
		c.record(ctx, key)
		c.promote(ctx, key)
	}
}

// settle gives a hold its draft once a generation held or the window passed, and reports whether the
// hold has one. Adoption comes first, so a draft whose Drafted write was lost never opens a second
// PR; then the catalog, since the curator would throw a verified fix away on a fingerprint hit.
func (c *Curation) settle(ctx context.Context, key string) bool {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil {
		return false
	}
	h := r.Curation
	if h.Drafted != "" {
		return !h.EntryDue || c.readEntry(ctx, key)
	}
	gens := generations(r)
	if !c.windowPassed(h) && !slices.ContainsFunc(gens, func(g Generation) bool { return g.Outcome() == "held" }) {
		return false
	}
	url, entry, found, err := c.adoptable(ctx, h.Fingerprint)
	if err != nil {
		// Past the window it never blocks the draft, however long it keeps failing.
		if ctx.Err() != nil {
			return false
		}
		if !c.windowPassed(h) {
			c.warn("looking for an open entry PR failed; the draft waits", key, err)
			return false
		}
		c.warn("looking for an open entry PR failed; the window has passed, so drafting", key, err)
	}
	if found {
		return c.writeDraft(key, url, entry, nil)
	}
	if path, hit := c.FindEntry(h.Fingerprint); hit && h.Fingerprint != "" {
		return c.writeDraft(key, draftedNone, path, nil)
	}
	return c.draftHeld(ctx, r, gens)
}

// adoptable finds the open RunLore PR that carries fingerprint. A PR with the marker that is not an
// entry RunLore opened is not adopted, whoever labelled it.
func (c *Curation) adoptable(ctx context.Context, fingerprint string) (url, entry string, found bool, err error) {
	if fingerprint == "" {
		return "", "", false, nil
	}
	prs, err := c.Drafts.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return "", "", false, err
	}
	for _, pr := range prs {
		if providers.ParseFingerprintMarker(pr.Body) != fingerprint {
			continue
		}
		pe, err := c.Drafts.PREntry(ctx, pr.Number)
		if errors.Is(err, providers.ErrNotRunLorePR) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		return pr.URL, pe.Path, true, nil
	}
	return "", "", false, nil
}

// draftHeld drafts the hold's entry through the curator's own gates, under the frozen fingerprint and
// with every generation that has an outcome.
func (c *Curation) draftHeld(ctx context.Context, r Record, gens []Generation) bool {
	key, h := r.TriggerKey, r.Curation
	var inv providers.Investigation
	if err := json.Unmarshal(h.Investigation, &inv); err != nil {
		c.warn("the hold's investigation is unreadable; no entry will be drafted", key, err)
		return c.writeDraft(key, draftedNone, "", nil)
	}
	carried := map[string]string{}
	for _, g := range gens {
		o := g.Outcome()
		if o == "" {
			continue
		}
		res, ready := c.resolution(ctx, key, g, c.windowPassed(h))
		if !ready {
			return false
		}
		if err := kbresolve.Validate(res); err != nil {
			c.warn("a fix cannot be written into the draft", key, err)
			continue
		}
		inv.Resolutions = append(inv.Resolutions, res)
		carried[g.FixRoom] = o
	}
	inv.PinnedFingerprint = h.Fingerprint
	var ref providers.Ref
	if c.Inner != nil {
		var err error
		if ref, err = c.Inner.Curate(ctx, inv); err != nil {
			if c.retries(ctx, h, stepDraft, "drafting the held entry failed", key, err) {
				return false
			}
			return c.writeDraft(key, draftedNone, "", nil)
		}
	}
	if ref.URL == "" {
		return c.writeDraft(key, draftedNone, "", nil)
	}
	// The URL is written before anything else can fail: OpenPR's label is best-effort, and an unlabelled
	// PR is in no listing, so losing the URL here would open a second PR. Only a hard kill between the
	// PR's creation and this write can still do that.
	if !c.writeDraft(key, ref.URL, "", carried) {
		return false
	}
	return c.readEntry(ctx, key)
}

// readEntry reads the entry path of the PR the hold drafted. The guard refuses a PR with no runlore
// label, and this one RunLore opened itself, so a refusal labels it first. Nothing is observed until
// the path is known, or its attempts past the window run out and the hold goes on with no entry to credit.
func (c *Curation) readEntry(ctx context.Context, key string) bool {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil {
		return false
	}
	h := r.Curation
	n, _ := prNumber(h.Drafted)
	pe, err := c.Drafts.PREntry(ctx, n)
	if errors.Is(err, providers.ErrNotRunLorePR) {
		if err = c.Drafts.ReplaceLabel(ctx, n, "", "runlore"); err == nil {
			pe, err = c.Drafts.PREntry(ctx, n)
		}
	}
	if err != nil && c.retries(ctx, h, stepEntry, "reading the drafted PR failed", key, err) {
		return false
	}
	return c.update(key, func(h *Hold) {
		h.Entry, h.EntryDue = pe.Path, false
		delete(h.PastWindowFailures, stepEntry)
	})
}

// writeDraft records the draft, once: Drafted never changes after this write, and Entry only from
// empty, when a URL comes with no entry and EntryDue asks for it. carried maps each fix room the draft
// wrote to its outcome.
func (c *Curation) writeDraft(key, drafted, entry string, carried map[string]string) bool {
	err := c.Ledger.Upsert(key, func(r *Record) {
		h := r.Curation
		if h == nil || h.Drafted != "" {
			return
		}
		h.Drafted, h.Entry, h.DraftedAt = drafted, entry, c.now()
		h.Investigation = nil // frozen from here on, and every write would carry it
		delete(h.PastWindowFailures, stepDraft)
		if drafted != draftedNone {
			h.LinkDue, h.PromoteDue, h.EntryDue = true, true, entry == ""
		}
		for room, o := range carried {
			if _, resolved := r.marks(room); resolved != nil {
				*resolved = o
			}
		}
	})
	if err != nil {
		c.warn("recording the draft failed; the next tick adopts it", key, err)
		return false
	}
	return true
}

// link gives the trigger its drafted PR, so a deferred PR is visible to Contested and to recurrence.
func (c *Curation) link(ctx context.Context, key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil || !r.Curation.LinkDue {
		return
	}
	err := c.Outcome.Link(key, r.Curation.Drafted, c.now())
	if err != nil && c.retries(ctx, r.Curation, stepLink, "linking the trigger to its draft failed", key, err) {
		return
	}
	c.update(key, func(h *Hold) {
		h.LinkDue = false
		delete(h.PastWindowFailures, stepLink)
	})
}

// observe credits the entry with each generation's new outcome. A Verify's At is the generation's own
// instant, the hold's acceptance plus its index, which a generation keeps once archived, never the
// clock: "latest" must mean the newest generation, and a late verdict on an older one must not outrank
// it. A replay carries the same instant and a flip of one room is not older, so both still fold.
func (c *Curation) observe(ctx context.Context, key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil {
		return
	}
	h := r.Curation
	for i, g := range generations(r) {
		o := g.Outcome()
		if o == "" || o == g.Observed {
			continue
		}
		if h.Entry != "" {
			if !c.stampInstant(key, h) {
				return
			}
			err := c.Outcome.Verify(outcome.Verification{
				Entry: h.Entry, DupFingerprint: h.Fingerprint, TriggerKey: key, Room: g.FixRoom, Held: o == "held", At: h.AcceptedAt.Add(time.Duration(i)),
			})
			if err != nil && c.retries(ctx, h, stepVerify+g.FixRoom, "recording the verification failed", key, err) {
				continue
			}
		}
		err := c.Ledger.Upsert(key, func(r *Record) {
			if observed, _ := r.marks(g.FixRoom); observed != nil {
				*observed = o
			}
			if r.Curation != nil {
				delete(r.Curation.PastWindowFailures, stepVerify+g.FixRoom)
				r.Curation.PromoteDue = r.Curation.PromoteDue || r.Curation.Drafted != draftedNone
			}
		})
		if err != nil {
			c.warn("recording the observation failed", key, err)
		}
	}
}

// stampInstant gives a hold written with no acceptance time, as a recall's is, the time of its first
// observation, durably and before any Verify uses it, so every later one reads the same instant.
func (c *Curation) stampInstant(key string, h *Hold) bool {
	if !h.AcceptedAt.IsZero() {
		return true
	}
	at := c.now()
	if !c.update(key, func(h *Hold) { h.AcceptedAt = at }) {
		return false
	}
	h.AcceptedAt = at
	return true
}

// record writes each generation's new outcome into the entry. A PR closed unmerged, one that is not
// RunLore's and a fix that cannot be named are terminal at once; any other failure goes through retries.
// A push strips ready-to-merge (KP20), so it asks for the promote again; a held fix that could
// not be written cancels the promote, since the label would land on a body without it. A dry-run
// neither calls the forge, every minute, nor marks a generation resolved.
func (c *Curation) record(ctx context.Context, key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil {
		return
	}
	h := r.Curation
	if h.ObserveOnly || h.Entry == "" || c.DryRun {
		return
	}
	target := kbresolve.Target{Path: h.Entry}
	if n, ok := prNumber(h.Drafted); ok {
		target = kbresolve.Target{PR: n}
	}
	for _, g := range generations(r) {
		o := g.Outcome()
		if o == "" || o == g.Resolved {
			continue
		}
		res, ready := c.resolution(ctx, key, g, c.windowPassed(h))
		if !ready {
			continue
		}
		written, retry := false, false
		route := ""
		if err := kbresolve.Validate(res); err != nil {
			c.warn("a fix cannot be written into the entry; not trying again", key, err)
		} else {
			got, err := c.Resolve(ctx, target, res)
			switch {
			case err == nil:
				written, route = true, got.Route
			case errors.Is(err, kbresolve.ErrClosedUnmerged), errors.Is(err, providers.ErrNotRunLorePR):
				c.warn("the entry's PR cannot take the fix; not trying again", key, err)
			default:
				retry = c.retries(ctx, h, stepResolve+g.FixRoom, "recording the fix on the entry failed", key, err)
			}
		}
		if retry {
			continue
		}
		err := c.Ledger.Upsert(key, func(r *Record) {
			if _, resolved := r.marks(g.FixRoom); resolved != nil {
				*resolved = o
			}
			if r.Curation == nil {
				return
			}
			delete(r.Curation.PastWindowFailures, stepResolve+g.FixRoom)
			switch {
			case route == "appended":
				r.Curation.PromoteDue = true
			case !written && o == "held":
				r.Curation.PromoteDue = false
			}
		})
		if err != nil {
			c.warn("recording the resolved outcome failed", key, err)
		}
	}
}

// promote runs the Queue's evidence check on the drafted PR, which labels or withdraws. It waits until
// every held generation is in the entry: a label laid before the push would be stripped by that push,
// with nothing to restore it until the next sweep. A failed generation does not wait, since withdrawing
// the label should not.
func (c *Curation) promote(ctx context.Context, key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil || !r.Curation.PromoteDue || c.Promote == nil {
		return
	}
	if slices.ContainsFunc(generations(r), func(g Generation) bool { return g.Outcome() == "held" && g.Resolved != "held" }) {
		return
	}
	if n, ok := prNumber(r.Curation.Drafted); ok {
		if err := c.Promote(ctx, n); err != nil && c.retries(ctx, r.Curation, stepPromote, "promoting the entry's PR failed", key, err) {
			return
		}
	}
	c.update(key, func(h *Hold) {
		h.PromoteDue = false
		delete(h.PastWindowFailures, stepPromote)
	})
}

// resolution is g's fix as written into an entry. ready is false while a passing forge failure should
// wait for a later tick: the marker hashes the fix and verdict only, so an entry written without the
// SHA after one blip would stay without it. A bad repo or no PR leaves FixPR empty, which Validate
// refuses.
func (c *Curation) resolution(ctx context.Context, key string, g Generation, windowPassed bool) (res providers.AppliedResolution, ready bool) {
	res = providers.AppliedResolution{Verdict: "resolved", Why: g.why(), Evidence: g.Evidence, VerifiedAt: cmp.Or(g.MonitorAt, c.now())}
	if g.Outcome() == "failed" {
		res.Verdict = "persisting"
	}
	repo := cmp.Or(g.Repo, c.Repository)
	if !validRepo(repo) || g.PR <= 0 {
		return res, true
	}
	res.FixPR = fmt.Sprintf("https://github.com/%s/pull/%d", repo, g.PR)
	sum, err := c.Pulls.PullSummary(ctx, repo, g.PR)
	switch {
	case err == nil:
		res.MergeSHA, res.Diff = sum.MergeSHA, diffLine(sum)
	case errors.Is(err, providers.ErrPullNotFound) || windowPassed:
		c.log().Info("writing a fix without its merge commit", "key", key, "pr", g.PR)
	default:
		c.warn("reading the merged fix failed; waiting for the next tick", key, err)
		return res, false
	}
	return res, true
}

// why is the reason a failed generation failed, the strongest signal first.
func (g Generation) why() string {
	switch {
	case g.Reverted:
		return "the fix was reverted"
	case g.Verdict == "persisting":
		return "the alert still fired after the soak"
	case g.Monitor == "regression":
		return "the factory's post-merge monitor read regression"
	}
	return ""
}

// diffLine is the entry's one line on what the fix changed: "1 file: a.yaml (+2 −2)". The forge lists
// at most five files, so five reads as a floor.
func diffLine(sum providers.PullSummary) string {
	count := "1 file"
	switch n := len(sum.Files); {
	case n == 0:
		return ""
	case n >= 5:
		count = "5+ files"
	case n > 1:
		count = strconv.Itoa(n) + " files"
	}
	return fmt.Sprintf("%s: %s (+%d −%d)", count, strings.Join(sum.Files, ", "), sum.Additions, sum.Deletions)
}

// generations is every generation of r with a fix, oldest first.
func generations(r Record) []Generation {
	gens := slices.Clone(r.Curation.Generations)
	if r.FixRoom != "" {
		gens = append(gens, r.Generation())
	}
	return gens
}

// marks points at what was last observed and last written for the generation of fix room room,
// current or archived; both are nil when r has no such generation.
func (r *Record) marks(room string) (observed, resolved *string) {
	if r.FixRoom == room {
		return &r.Observed, &r.Resolved
	}
	if r.Curation != nil {
		for i := range r.Curation.Generations {
			if g := &r.Curation.Generations[i]; g.FixRoom == room {
				return &g.Observed, &g.Resolved
			}
		}
	}
	return nil, nil
}

// update writes to key's hold, when it still has one, and reports whether the write landed.
func (c *Curation) update(key string, fn func(*Hold)) bool {
	err := c.Ledger.Upsert(key, func(r *Record) {
		if r.Curation != nil {
			fn(r.Curation)
		}
	})
	if err != nil {
		c.warn("updating the hold failed", key, err)
		return false
	}
	return true
}

func (c *Curation) windowPassed(h *Hold) bool {
	return !c.now().Before(h.AcceptedAt.Add(c.Window))
}

// prNumber is the number a pull request's URL ends in.
func prNumber(url string) (int, bool) {
	n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	return n, err == nil && n > 0
}

// The steps Hold.PastWindowFailures counts; verify and resolve count per fix room, so two failing
// generations do not spend one budget.
const (
	stepDraft   = "draft"
	stepEntry   = "entry"
	stepLink    = "link"
	stepVerify  = "verify:"
	stepResolve = "resolve:"
	stepPromote = "promote"
)

// retries reports whether a failed step should try again on the next tick. Within the window it always
// does. Past it, the step's attempts are counted on the hold, and the one that reaches
// maxPastWindowFailures is terminal: it is logged here, once, and the caller settles the step, which
// clears the count, as a success does. A cancelled context says nothing about the failure and counts for
// nothing: leadership moved or the process is ending. A count that cannot be written retries too.
func (c *Curation) retries(ctx context.Context, h *Hold, step, msg, key string, err error) bool {
	switch {
	case ctx.Err() != nil:
		return true
	case !c.windowPassed(h):
		c.warn(msg+"; retrying on the next tick", key, err)
		return true
	}
	failures := 0
	written := c.update(key, func(h *Hold) {
		if h.PastWindowFailures == nil {
			h.PastWindowFailures = map[string]int{}
		}
		h.PastWindowFailures[step]++
		failures = h.PastWindowFailures[step]
		if failures >= maxPastWindowFailures {
			delete(h.PastWindowFailures, step)
		}
	})
	if !written || failures < maxPastWindowFailures {
		c.warn(msg+"; retrying on the next tick", key, err)
		return true
	}
	c.log().Warn(msg+"; not trying again", "key", key, "reason", "window_passed", "failures", failures, "err", redact.Secrets(err.Error()))
	return false
}

func (c *Curation) warn(msg, key string, err error) {
	c.log().Warn(msg, "key", key, "err", redact.Secrets(err.Error()))
}
