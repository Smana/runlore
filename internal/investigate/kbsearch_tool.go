// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// KBSearchTool lets the model search the OKF knowledge catalog (runbooks, past
// incidents) to ground its reasoning.
type KBSearchTool struct {
	Catalog catalog.Searcher

	// hits, when set, records the strongest clear-match kb_search hit this
	// investigation saw so the loop can surface it on the delivered finding
	// (Investigation.MatchedKnowledge). Per-investigation: the loop rebinds a fresh
	// copy of the tool via withHitTracker; the shared li.Tools copy leaves it nil, so
	// a bare KBSearchTool captures nothing.
	hits *kbHitTracker
}

// kbClearMatchScore is the BM25 bar a kb_search hit's top score must clear to count
// as a "clear match" worth surfacing on the notification. It mirrors instant recall's
// default SoloFloor (config load default 4.0) — the score at which recall trusts a
// LONE BM25 hit enough to short-circuit the whole investigation. The reasoning: if a
// hit is strong enough that recall would have delivered it as the answer on its own,
// it is unquestionably strong enough to tell the on-call "we already have a runbook
// for this". Surfacing (a low-stakes hint on an investigation that still ran in full)
// is a strictly weaker claim than recall's short-circuit, so borrowing recall's most
// conservative single-hit bar keeps this high-precision — no noise from weak,
// tangentially-relevant hits. BM25 scores are corpus-dependent; recording Score on
// every match lets this be tuned from live data exactly as the recall thresholds are.
const kbClearMatchScore = 4.0

// kbHitTracker accumulates the single strongest clear-match kb_search hit across an
// investigation's tool calls. An assistant turn's tool calls run concurrently
// (dispatchTools), and the model may issue several kb_search calls, so observe is
// mutex-guarded — the -race gate covers this.
type kbHitTracker struct {
	mu   sync.Mutex
	best *providers.MatchedEntry
}

// observe keeps e when it out-scores the current best (or there is none yet). It
// stores a copy so the caller's value can't be mutated after the fact.
func (t *kbHitTracker) observe(e providers.MatchedEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.best == nil || e.Score > t.best.Score {
		cp := e
		t.best = &cp
	}
}

// top returns the strongest recorded clear-match hit, or nil when none cleared the
// bar. The returned pointer is the tracker's own copy; the loop treats it as read-only.
func (t *kbHitTracker) top() *providers.MatchedEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.best
}

// withHitTracker returns a copy of the tool that records its strongest clear-match hit
// into tr. Used per investigation so the shared li.Tools copy is never given a tracker.
func (t KBSearchTool) withHitTracker(tr *kbHitTracker) KBSearchTool {
	t.hits = tr
	return t
}

// Name returns the tool name.
func (t KBSearchTool) Name() string { return "kb_search" }

// Description returns the tool description.
func (t KBSearchTool) Description() string {
	return "Search the knowledge catalog (runbooks, past incidents) for entries relevant to a query."
}

// Schema returns the JSON schema for the arguments.
func (t KBSearchTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`
}

// Call searches the catalog and renders the top matches. When the catalog exposes
// relevance scores (catalog.ScoredSearcher — the production *catalog.Catalog does),
// the scored search is preferred so the loop can capture the strongest clear-match
// hit for the notification; a plain Searcher falls back to the unscored path.
func (t KBSearchTool) Call(_ context.Context, args string) (string, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if ss, ok := t.Catalog.(catalog.ScoredSearcher); ok {
		scored, err := ss.SearchScored(in.Query, 3)
		if err != nil {
			return "", err
		}
		t.observeTopHit(scored)
		if len(scored) == 0 {
			return "no matching catalog entries", nil
		}
		hits := make([]catalog.Entry, len(scored))
		for i, s := range scored {
			hits[i] = s.Entry
		}
		return renderHits(hits), nil
	}
	hits, err := t.Catalog.Search(in.Query, 3)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no matching catalog entries", nil
	}
	return renderHits(hits), nil
}

// observeTopHit records the top hit into the tracker when it clears the clear-match
// bar. SearchScored returns hits in descending score order, so scored[0] is the top.
// No-op when no tracker is bound (a bare tool) or nothing cleared the bar.
func (t KBSearchTool) observeTopHit(scored []catalog.ScoredEntry) {
	if t.hits == nil || len(scored) == 0 {
		return
	}
	top := scored[0]
	if top.Score < kbClearMatchScore {
		return // not confidently a known-runbook match — surfacing it would be noise
	}
	// Carry Path + Title (+ Score). URL is deliberately left empty: the entry's web
	// link needs the forge repo, which isn't reachable from the tool without new
	// plumbing — the notifier shows Path instead (see MatchedEntry).
	t.hits.observe(providers.MatchedEntry{
		Path:  top.Entry.Path,
		Title: top.Entry.Title,
		Score: top.Score,
	})
}

// renderHits formats catalog hits into the markdown the model reads.
func renderHits(hits []catalog.Entry) string {
	var b strings.Builder
	for _, e := range hits {
		fmt.Fprintf(&b, "## %s  (%s)\n%s\n%s\n\n", e.Title, e.Path, e.Description, e.Body)
	}
	return b.String()
}
