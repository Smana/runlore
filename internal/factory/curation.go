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
	// DryRun is true when Resolve's forge writes nothing, so no generation may be marked resolved.
	DryRun bool
	Now    func() time.Time
	Log    *slog.Logger

	once sync.Mutex // a leadership change can start a tick while the old one ends
}

// Curate files inv with the factory. On a 201, or a 200 with no hold yet, it holds the entry and
// drafts nothing; any other answer takes today's path at once, unless a hold already owns the
// incident's entry. A 200 with a hold standing is no new generation and touches nothing. A recall is
// filed and observed, never drafted.
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
	h := c.standingHold(inv.TriggerKey)
	owned := h != nil && !h.ObserveOnly
	switch {
	case owned && (!accepted || h.Drafted != ""):
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
	if err := c.Ledger.Upsert(inv.TriggerKey, func(r *Record) { ref = c.hold(r, inv, snapshot) }); err != nil {
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
// for good, and the verified fix with it.
func (c *Curation) hold(r *Record, inv providers.Investigation, snapshot []byte) providers.Ref {
	switch h := r.Curation; {
	case h == nil || h.ObserveOnly:
		fresh := &Hold{AcceptedAt: c.now(), Investigation: snapshot, Fingerprint: cmp.Or(r.DraftFingerprint, curator.DupFingerprint(inv))}
		if h != nil {
			fresh.Generations = h.Generations
		}
		r.Curation = fresh
	case h.Drafted == "":
		if c.meetsBar(inv) {
			h.Investigation = snapshot
		}
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
// room, and Resolve is idempotent.
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
		c.link(key)
		c.observe(key)
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
		return true
	}
	gens := generations(r)
	if !c.windowPassed(h) && !slices.ContainsFunc(gens, func(g Generation) bool { return g.Outcome() == "held" }) {
		return false
	}
	url, entry, found, err := c.adoptable(ctx, h.Fingerprint)
	if err != nil {
		c.warn("listing the open entry PRs failed; the draft waits", key, err)
		return false
	}
	if found {
		return c.writeDraft(key, url, entry, nil)
	}
	if path, hit := c.FindEntry(h.Fingerprint); hit {
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
			c.warn("drafting the held entry failed; retrying on the next tick", key, err)
			return false
		}
	}
	if ref.URL == "" {
		return c.writeDraft(key, draftedNone, "", nil)
	}
	n, _ := prNumber(ref.URL)
	pe, err := c.Drafts.PREntry(ctx, n)
	switch {
	case errors.Is(err, providers.ErrNotRunLorePR):
		// Its label failed, so no listing would find it: keep the URL, or the next tick opens a second PR.
		c.warn("the drafted PR is refused as RunLore's; its entry goes unobserved", key, err)
	case err != nil:
		c.warn("reading the drafted PR failed; the next tick adopts it", key, err)
		return false
	}
	return c.writeDraft(key, ref.URL, pe.Path, carried)
}

// writeDraft records the draft, once: Drafted and Entry never change after this write. carried maps
// each fix room the draft wrote to its outcome.
func (c *Curation) writeDraft(key, drafted, entry string, carried map[string]string) bool {
	err := c.Ledger.Upsert(key, func(r *Record) {
		h := r.Curation
		if h == nil || h.Drafted != "" {
			return
		}
		h.Drafted, h.Entry, h.DraftedAt = drafted, entry, c.now()
		if drafted != draftedNone {
			h.LinkDue, h.PromoteDue = true, true
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
func (c *Curation) link(key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil || !r.Curation.LinkDue {
		return
	}
	if err := c.Outcome.Link(key, r.Curation.Drafted, c.now()); err != nil {
		c.warn("linking the trigger to its draft failed; retrying on the next tick", key, err)
		return
	}
	c.update(key, func(h *Hold) { h.LinkDue = false })
}

// observe credits the entry with each generation's new outcome. Observations of one tick are ordered
// by generation: Verifications lets a failure win a tie, which would hide the fix that held after it.
func (c *Curation) observe(key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil {
		return
	}
	h := r.Curation
	at := c.now()
	for i, g := range generations(r) {
		o := g.Outcome()
		if o == "" || o == g.Observed {
			continue
		}
		if h.Entry != "" {
			err := c.Outcome.Verify(outcome.Verification{
				Entry: h.Entry, DupFingerprint: h.Fingerprint, TriggerKey: key, Room: g.FixRoom, Held: o == "held", At: at.Add(time.Duration(i)),
			})
			if err != nil {
				c.warn("recording the verification failed; retrying on the next tick", key, err)
				continue
			}
		}
		err := c.Ledger.Upsert(key, func(r *Record) {
			if observed, _ := r.marks(g.FixRoom); observed != nil {
				*observed = o
			}
			if r.Curation != nil && r.Curation.Drafted != draftedNone {
				r.Curation.PromoteDue = true
			}
		})
		if err != nil {
			c.warn("recording the observation failed", key, err)
		}
	}
}

// record writes each generation's new outcome into the entry. A PR closed unmerged, one that is not
// RunLore's and a fix that cannot be named are terminal; any other failure retries on the next tick.
// A dry-run neither calls the forge, every minute, nor marks a generation resolved.
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
		if err := kbresolve.Validate(res); err != nil {
			c.warn("a fix cannot be written into the entry; not trying again", key, err)
		} else if _, err := c.Resolve(ctx, target, res); err != nil {
			if !errors.Is(err, kbresolve.ErrClosedUnmerged) && !errors.Is(err, providers.ErrNotRunLorePR) {
				c.warn("recording the fix on the entry failed; retrying on the next tick", key, err)
				continue
			}
			c.warn("the entry's PR cannot take the fix; not trying again", key, err)
		}
		err := c.Ledger.Upsert(key, func(r *Record) {
			if _, resolved := r.marks(g.FixRoom); resolved != nil {
				*resolved = o
			}
		})
		if err != nil {
			c.warn("recording the resolved outcome failed", key, err)
		}
	}
}

// promote runs the Queue's evidence check on the drafted PR, which labels or withdraws.
func (c *Curation) promote(ctx context.Context, key string) {
	r, ok := c.Ledger.Get(key)
	if !ok || r.Curation == nil || !r.Curation.PromoteDue || c.Promote == nil {
		return
	}
	if n, ok := prNumber(r.Curation.Drafted); ok {
		if err := c.Promote(ctx, n); err != nil {
			c.warn("promoting the entry's PR failed; retrying on the next tick", key, err)
			return
		}
	}
	c.update(key, func(h *Hold) { h.PromoteDue = false })
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

// update writes to key's hold, when it still has one.
func (c *Curation) update(key string, fn func(*Hold)) {
	err := c.Ledger.Upsert(key, func(r *Record) {
		if r.Curation != nil {
			fn(r.Curation)
		}
	})
	if err != nil {
		c.warn("updating the hold failed", key, err)
	}
}

func (c *Curation) windowPassed(h *Hold) bool {
	return !c.now().Before(h.AcceptedAt.Add(c.Window))
}

// prNumber is the number a pull request's URL ends in.
func prNumber(url string) (int, bool) {
	n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	return n, err == nil && n > 0
}

func (c *Curation) warn(msg, key string, err error) {
	c.log().Warn(msg, "key", key, "err", redact.Secrets(err.Error()))
}
