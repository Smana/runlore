package investigate

import (
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

const (
	// compactionBudgetFraction is the fraction of MaxTokensPerInvestigation at which
	// compaction triggers and the size it elides back down to (headroom for more steps).
	compactionBudgetFraction = 0.7
	// keepRecentToolOutputs is the number of most-recent tool results kept verbatim
	// (the model's active working set).
	keepRecentToolOutputs = 3
)

// Compaction modes (config investigation.compaction). Empty is treated as elide.
const (
	compactionElide     = "elide"     // drop elided tool-output bodies for markers (lossy; default)
	compactionSummarize = "summarize" // replace the elided batch with one model-produced digest
)

// keepListTools are tools whose outputs are the structural root-cause skeleton and are
// never elided: the change timeline, the runbook hit, the failing resource's status, and
// the dependency-cascade root. The gitops_* pair are engine-agnostic (Flux + ArgoCD).
var keepListTools = map[string]bool{
	"what_changed":           true,
	"kb_search":              true,
	"gitops_resource_status": true,
	"gitops_tree":            true,
}

const (
	elidedPrefix = "[earlier "
	elidedSuffix = " output elided to bound context]"
)

func elidedMarker(tool string) string {
	if tool == "" {
		tool = "tool"
	}
	return elidedPrefix + tool + elidedSuffix
}

func isElidedMarker(s string) bool {
	return strings.HasPrefix(s, elidedPrefix) && strings.HasSuffix(s, elidedSuffix)
}

// compactionTarget returns the estimate at/below which compaction stops: 0.7 * budget.
// budget <= 0 disables compaction (returns 0).
func compactionTarget(budget int) int {
	if budget <= 0 {
		return 0
	}
	return int(float64(budget) * compactionBudgetFraction)
}

// elidedOutput records one tool-result body that compaction removed: the tool that
// produced it, its original (already-redacted) content, and its position in the
// RETURNED slice — so the summarize path can batch the bodies for a digest and know
// where to write it back.
type elidedOutput struct {
	tool    string
	content string
	mi      int
}

// compactHistory elides the bodies of eligible tool-result messages — superseded ones
// first, then largest-first — until estimateTokens(sys, messages, specs) drops to or
// below target, or no eligible output remains. Protected: the seed (index 0), every
// assistant turn, keep-list tool results, and the most-recent keepRecentToolOutputs tool
// results. Returns a new slice (the caller's messages are never mutated) and the number
// of body bytes elided (0 when nothing was compacted). target <= 0 is a no-op.
//
// This is the elide-mode entry point (default): it discards the elidedOutput detail
// that the summarize path (compactHistoryDetailed) consumes.
func compactHistory(messages []providers.Message, sys string, specs []providers.ToolSpec, target int) ([]providers.Message, int) {
	out, elided, _ := compactHistoryDetailed(messages, sys, specs, target)
	return out, elided
}

// compactHistoryDetailed is compactHistory's core: identical selection and elision,
// but it also returns, in elision order, the bodies it removed (tool name + original
// content + their index in the returned slice). The summarize path uses those to ask
// for one digest and write it back over the markers. Behaviour for the returned
// (messages, elided) pair is byte-for-byte identical to plain elision.
func compactHistoryDetailed(messages []providers.Message, sys string, specs []providers.ToolSpec, target int) ([]providers.Message, int, []elidedOutput) {
	if target <= 0 || estimateTokens(sys, messages, specs) <= target {
		return messages, 0, nil
	}
	// Resolve each tool-call id -> (name, args) so a tool RESULT (which carries only
	// ToolCallID) is attributable to its tool and dedupable by (name, args).
	type call struct{ name, args string }
	byID := map[string]call{}
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			byID[tc.ID] = call{tc.Name, tc.Args}
		}
	}
	// Positions of tool-result messages, in order.
	var toolIdx []int
	for i, m := range messages {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	recentCut := len(toolIdx) - keepRecentToolOutputs // list-positions >= this are recency-protected
	// Last list-position per (name, args) — earlier ones are superseded.
	lastPosFor := map[call]int{}
	for pos, mi := range toolIdx {
		lastPosFor[byID[messages[mi].ToolCallID]] = pos
	}
	type cand struct {
		mi         int
		size       int
		superseded bool
	}
	var cands []cand
	for pos, mi := range toolIdx {
		if pos >= recentCut {
			continue // most-recent K
		}
		c := byID[messages[mi].ToolCallID]
		if keepListTools[c.name] || isElidedMarker(messages[mi].Content) {
			continue
		}
		if len(messages[mi].Content) <= len(elidedMarker(c.name)) {
			continue // body no larger than the marker — eliding wouldn't shrink it
		}
		cands = append(cands, cand{mi: mi, size: len(messages[mi].Content), superseded: lastPosFor[c] != pos})
	}
	// Superseded first, then largest-first.
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].superseded != cands[b].superseded {
			return cands[a].superseded
		}
		return cands[a].size > cands[b].size
	})
	// Copy so the caller's slice contents are never mutated.
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	elided := 0
	var removed []elidedOutput
	for _, cd := range cands {
		if estimateTokens(sys, out, specs) <= target {
			break
		}
		name := byID[out[cd.mi].ToolCallID].name
		before := out[cd.mi].Content
		out[cd.mi].Content = elidedMarker(name)
		elided += len(before) - len(out[cd.mi].Content)
		removed = append(removed, elidedOutput{tool: name, content: before, mi: cd.mi})
	}
	if elided == 0 {
		return messages, 0, nil
	}
	return out, elided, removed
}
