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

	// enrich, when non-empty, is appended SERVER-SIDE to every model-issued query
	// before it hits the index (see kbSearchEnrichment). It is the request's
	// discriminating structured entity — normalized workload ref + alertname — the
	// exact expansion buildRecallQuery folds in to fix the measured 0.096→rank-1
	// vocabulary mismatch (recall.go). Set per investigation via withEnrichment; the
	// shared li.Tools copy leaves it empty, so a bare tool searches raw text.
	enrich string
}

// kbClearMatchScoreDefault is the FALLBACK BM25 bar a kb_search hit's top score must
// clear to count as a "clear match" worth surfacing on the notification. It is used only
// when instant recall is disabled/unconfigured, so there is no configured floor to
// borrow; when recall IS configured the per-investigation tracker instead tracks the
// operator's CONFIGURED SoloFloor (see newKBHitTracker + the app wiring), keeping the
// visibility bar in the same BM25 score regime kb_search actually runs in.
//
// Why the SoloFloor is the right thing to track: it is the score at which recall trusts a
// LONE BM25 hit enough to short-circuit the whole investigation. If a hit is strong
// enough that recall would have delivered it as the answer on its own, it is
// unquestionably strong enough to tell the on-call "we already have a runbook for this".
// Surfacing (a low-stakes hint on an investigation that still ran in full) is a strictly
// WEAKER claim than recall's short-circuit, so borrowing recall's most conservative
// single-hit bar keeps this high-precision — no noise from weak, tangential hits.
//
// The bar MUST track config because BM25 scores are corpus/query-dependent: a real
// Alertmanager-driven cluster whose label-derived alert queries score ~0.1–0.3 tunes
// solo_floor DOWN (observed live: 0.2). A hardcoded 4.0 would make this visibility signal
// a silent no-op on exactly those clusters — kb_search finds the runbook, yet the "Matches
// known runbook" block could never show (live-found). Score is recorded on every match so
// the bar can be tuned from live data exactly as the recall thresholds are. The default is
// kept at 4.0 (instant recall's default SoloFloor) so behaviour is unchanged when nothing
// is configured.
const kbClearMatchScoreDefault = 4.0

// kbHitTracker accumulates the single strongest clear-match kb_search hit across an
// investigation's tool calls. An assistant turn's tool calls run concurrently
// (dispatchTools), and the model may issue several kb_search calls, so observe is
// mutex-guarded — the -race gate covers this.
type kbHitTracker struct {
	mu   sync.Mutex
	best *providers.MatchedEntry
	// clearMatchScore is the BM25 bar the top hit must clear to be recorded. It is set
	// per investigation (see newKBHitTracker) from the operator's configured recall
	// SoloFloor, so the visibility bar auto-adapts to the cluster's BM25 scale instead of
	// being pinned to a hardcoded 4.0. Read-only after construction, so the mutex above
	// (which guards best) need not cover it.
	clearMatchScore float64
}

// newKBHitTracker builds a per-investigation tracker whose clear-match bar is
// clearMatchScore. A non-positive value — instant recall disabled/unconfigured, so there
// is no configured SoloFloor to borrow — falls back to kbClearMatchScoreDefault, keeping
// the historical 4.0 bar so behaviour is unchanged when nothing is configured.
func newKBHitTracker(clearMatchScore float64) *kbHitTracker {
	if clearMatchScore <= 0 {
		clearMatchScore = kbClearMatchScoreDefault
	}
	return &kbHitTracker{clearMatchScore: clearMatchScore}
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

// withEnrichment returns a copy of the tool that appends s to every model-issued
// query server-side. Used per investigation (see the loop's per-request rebinding)
// so the shared li.Tools copy searches raw text; an empty s is a no-op.
func (t KBSearchTool) withEnrichment(s string) KBSearchTool {
	t.enrich = s
	return t
}

// kbSearchEnrichment builds the server-side query suffix for a request's kb_search
// calls: the discriminating structured entity — namespace, normalized workload
// name, and alertname — the SAME expansion buildRecallQuery folds in front of BM25.
// WHY: a label-derived alert's title/message is 1–2 generic tokens; the object that
// identifies the incident (namespace/name) lives only in the labels, so the model's
// symptom-text kb_search re-suffers the vocabulary mismatch recall already measured
// and solved (0.096 BM25 vs a perfect runbook → rank #1 once the ref is folded in;
// see buildRecallQuery). The name is normalized (pod-hash stripped) so a per-pod
// alert reaches the controller-family runbook; empty parts are dropped so the suffix
// never carries stray whitespace. It is ADDITIVE — appended to the model's own
// query, never replacing it — so a query that already names the workload is at worst
// neutral.
func kbSearchEnrichment(req Request) string {
	parts := make([]string, 0, 3)
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	add(req.Workload.Namespace)
	add(providers.NormalizeWorkloadName(req.Workload.Name))
	add(req.Labels["alertname"])
	return strings.Join(parts, " ")
}

// Name returns the tool name.
func (t KBSearchTool) Name() string { return "kb_search" }

// Description returns the tool description. When the tool is bound to a request's
// enrichment (per investigation), the description states that the incident's
// workload/alert context is folded into the search server-side, so the model knows
// it need not repeat those terms and can query the SYMPTOM plainly.
func (t KBSearchTool) Description() string {
	base := "Search the knowledge catalog (runbooks, past incidents) for entries relevant to a query."
	if t.enrich != "" {
		base += " The failing workload and alert name are automatically added to your query, so search by symptom."
	}
	return base
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
	// Server-side enrichment: fold the request's workload ref + alertname into the
	// model's query so kb_search inherits recall's measured 0.096→rank-1 fix. Additive
	// (model query first, then the entity terms); a no-op when unbound (bare tool).
	query := in.Query
	if t.enrich != "" {
		if q := strings.TrimSpace(query); q != "" {
			query = q + " " + t.enrich
		} else {
			query = t.enrich
		}
	}
	if ss, ok := t.Catalog.(catalog.ScoredSearcher); ok {
		scored, err := ss.SearchScored(query, 3)
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
	hits, err := t.Catalog.Search(query, 3)
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
	if top.Score < t.hits.clearMatchScore {
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
