package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// toolMsg builds an assistant tool-call turn followed by its tool-result message.
func callAndResult(id, name, args, result string) []providers.Message {
	return []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: id, Name: name, Args: args}}},
		{Role: "tool", ToolCallID: id, Content: result},
	}
}

func buildHistory(seedSize int, calls ...[]providers.Message) []providers.Message {
	msgs := []providers.Message{{Role: "user", Content: strings.Repeat("s", seedSize)}}
	for _, c := range calls {
		msgs = append(msgs, c...)
	}
	return msgs
}

func toolResultByID(msgs []providers.Message, id string) string {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

func TestCompactElidesOldBeyondRecentK(t *testing.T) {
	big := strings.Repeat("x", 4000)
	// 5 pod_logs calls; with K=3, the oldest 2 (ids 1,2) are eligible.
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{"ns":"a"}`, big),
		callAndResult("2", "pod_logs", `{"ns":"b"}`, big),
		callAndResult("3", "pod_logs", `{"ns":"c"}`, big),
		callAndResult("4", "pod_logs", `{"ns":"d"}`, big),
		callAndResult("5", "pod_logs", `{"ns":"e"}`, big),
	)
	target := estimateTokens("", msgs, nil) - 1500 // force some elision
	out, elided := compactHistory(msgs, "", nil, target)
	if elided == 0 {
		t.Fatal("expected some bytes elided")
	}
	// recent 3 (ids 3,4,5) kept verbatim
	for _, id := range []string{"3", "4", "5"} {
		if toolResultByID(out, id) != big {
			t.Fatalf("recent tool result %s must be kept verbatim", id)
		}
	}
	// oldest (id 1) elided to a marker
	if !isElidedMarker(toolResultByID(out, "1")) {
		t.Fatalf("oldest tool result should be elided, got %q", toolResultByID(out, "1"))
	}
}

func TestCompactProtectsSeedAssistantAndKeepList(t *testing.T) {
	big := strings.Repeat("y", 5000)
	msgs := buildHistory(20,
		callAndResult("1", "what_changed", `{}`, big),           // keep-listed
		callAndResult("2", "kb_search", `{}`, big),              // keep-listed
		callAndResult("3", "gitops_tree", `{}`, big),            // keep-listed
		callAndResult("4", "gitops_resource_status", `{}`, big), // keep-listed
		callAndResult("5", "pod_logs", `{}`, big),
		callAndResult("6", "pod_logs", `{}`, big),
		callAndResult("7", "pod_logs", `{}`, big),
		callAndResult("8", "pod_logs", `{}`, big),
	)
	target := 1 // force maximum elision
	out, _ := compactHistory(msgs, "", nil, target)
	// keep-list tools never elided
	for _, id := range []string{"1", "2", "3", "4"} {
		if toolResultByID(out, id) != big {
			t.Fatalf("keep-list tool %s must never be elided", id)
		}
	}
	// seed untouched
	if out[0].Content != strings.Repeat("s", 20) {
		t.Fatal("seed must never be elided")
	}
	// assistant turns untouched (still carry their tool calls)
	for _, m := range out {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			t.Fatal("assistant tool-call turn must be preserved")
		}
	}
}

func TestCompactSupersededFirst(t *testing.T) {
	smallSuperseded := strings.Repeat("z", 2400) // id1: superseded, SMALLER
	largeOneOff := strings.Repeat("z", 4000)     // id2: not superseded, LARGER
	big := strings.Repeat("z", 2000)
	// 5 tool results -> recentCut = 2, so positions 0 (id1) and 1 (id2) are both eligible.
	// id1 (pod_logs ns=a) is superseded by id5 (same name+args, later). id2 is a larger
	// one-off. Superseded-first must elide the SMALLER id1 before the LARGER id2.
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{"ns":"a"}`, smallSuperseded),
		callAndResult("2", "controller_logs", `{"c":"x"}`, largeOneOff),
		callAndResult("3", "pod_logs", `{"ns":"b"}`, big),
		callAndResult("4", "pod_logs", `{"ns":"d"}`, big),
		callAndResult("5", "pod_logs", `{"ns":"a"}`, big), // re-query of id1 -> supersedes it
	)
	target := estimateTokens("", msgs, nil) - 400 // one elision (~590 tokens) drops under it
	out, _ := compactHistory(msgs, "", nil, target)
	if !isElidedMarker(toolResultByID(out, "1")) {
		t.Fatal("superseded id 1 should be elided first, even though it is smaller")
	}
	if isElidedMarker(toolResultByID(out, "2")) {
		t.Fatal("larger non-superseded id 2 must NOT be elided when one elision sufficed")
	}
}

func TestCompactNoopWhenUnderTarget(t *testing.T) {
	msgs := buildHistory(10, callAndResult("1", "pod_logs", `{}`, "small"))
	target := estimateTokens("", msgs, nil) + 1000 // already under
	out, elided := compactHistory(msgs, "", nil, target)
	if elided != 0 {
		t.Fatalf("expected no-op, elided %d", elided)
	}
	if len(out) == 0 {
		t.Fatal("should return the history")
	}
}

func TestCompactDisabledTargetZero(t *testing.T) {
	msgs := buildHistory(10, callAndResult("1", "pod_logs", `{}`, strings.Repeat("x", 9000)))
	_, elided := compactHistory(msgs, "", nil, 0)
	if elided != 0 {
		t.Fatal("target<=0 must be a no-op")
	}
}

func TestCompactIdempotent(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{}`, big),
		callAndResult("2", "pod_logs", `{}`, big),
		callAndResult("3", "pod_logs", `{}`, big),
		callAndResult("4", "pod_logs", `{}`, big),
	)
	target := estimateTokens("", msgs, nil) - 1000
	once, _ := compactHistory(msgs, "", nil, target)
	twice, elided2 := compactHistory(once, "", nil, target)
	if elided2 != 0 {
		t.Fatalf("second compaction pass should be a no-op, elided %d", elided2)
	}
	_ = twice
}

func TestCompactSkipsBodiesNoLargerThanMarker(t *testing.T) {
	tiny := "x" // far smaller than the ~49-byte marker
	big := strings.Repeat("b", 5000)
	// 5 tool results -> recentCut = 2; positions 0 (tiny, id1) and 1 (big, id2) are eligible.
	msgs := buildHistory(10,
		callAndResult("1", "pod_logs", `{"ns":"a"}`, tiny),
		callAndResult("2", "pod_logs", `{"ns":"b"}`, big),
		callAndResult("3", "pod_logs", `{"ns":"c"}`, big),
		callAndResult("4", "pod_logs", `{"ns":"d"}`, big),
		callAndResult("5", "pod_logs", `{"ns":"e"}`, big),
	)
	out, _ := compactHistory(msgs, "", nil, 1) // target=1 forces maximum elision
	if toolResultByID(out, "1") != tiny {
		t.Fatalf("a body smaller than the marker must be left untouched, got %q", toolResultByID(out, "1"))
	}
}

func TestCompactionTarget(t *testing.T) {
	if compactionTarget(0) != 0 || compactionTarget(-5) != 0 {
		t.Fatal("budget<=0 disables compaction")
	}
	if got := compactionTarget(100000); got != 70000 {
		t.Fatalf("compactionTarget(100000)=%d, want 70000", got)
	}
}
