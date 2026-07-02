package investigate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// fakeSummarizer is a stand-in for the digest model: it records every call (so a
// test can assert exactly one summarization per compaction event) and the review
// content it saw, and returns a scripted response/error.
type fakeSummarizer struct {
	resp providers.CompletionResponse
	err  error

	mu       sync.Mutex
	calls    int
	sawSys   string
	sawInput string
}

func (f *fakeSummarizer) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	f.mu.Lock()
	f.calls++
	f.sawSys = req.System
	if len(req.Messages) > 0 {
		f.sawInput = req.Messages[0].Content
	}
	f.mu.Unlock()
	return f.resp, f.err
}

func (f *fakeSummarizer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// summarizeLoop builds a loop that calls a big-output tool six times under a low
// token budget (so compaction fires) then submits findings, with summarize mode and
// the given summarizer wired as VerifyModel.
func summarizeLoop(t *testing.T, sm providers.ModelProvider, m *telemetry.Metrics) (*LoopInvestigator, *providers.Investigation) {
	t.Helper()
	var resp []providers.CompletionResponse
	for i := 1; i <= 6; i++ {
		resp = append(resp, providers.CompletionResponse{
			ToolCalls: []providers.ToolCall{{ID: fmtID(i), Name: "big_tool", Args: `{}`}},
		})
	}
	resp = append(resp, providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "f", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"found it"}]}`},
	}})
	got := new(providers.Investigation)
	li := &LoopInvestigator{
		Model:                     &scriptModel{responses: resp},
		VerifyModel:               sm,
		Tools:                     []Tool{bigTool{size: 4000}},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 6000,
		Compaction:                "summarize",
		ModelProvider:             "anthropic",
		Metrics:                   m,
		OnComplete:                func(inv providers.Investigation) { *got = inv },
	}
	return li, got
}

const digestSentinel = "SUMMARIZED-DIGEST-SENTINEL"

// msgsContain reports whether any request in the script model saw a message whose
// content contains sub.
func msgsContain(reqs []providers.CompletionRequest, sub string) bool {
	for _, r := range reqs {
		for _, msg := range r.Messages {
			if strings.Contains(msg.Content, sub) {
				return true
			}
		}
	}
	return false
}

// TestSummarizeInsertsDigest proves summarize mode replaces the elided batch with the
// model's digest (clearly labelled) in the history the MAIN model subsequently sees,
// and that the summarizer was prompted with the elided tool output.
func TestSummarizeInsertsDigest(t *testing.T) {
	sm := &fakeSummarizer{resp: providers.CompletionResponse{Text: digestSentinel + " harbor CrashLoopBackOff x3"}}
	li, got := summarizeLoop(t, sm, nil)
	if err := li.Investigate(context.Background(), Request{Title: "summarize insert"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(got.RootCauses) == 0 {
		t.Fatalf("investigation did not finish: %+v", got)
	}
	if sm.count() == 0 {
		t.Fatal("summarizer was never called under summarize mode")
	}
	model := li.Model.(*scriptModel)
	if !msgsContain(model.reqs, digestSentinel) {
		t.Fatal("digest text never reached the main model's history")
	}
	if !msgsContain(model.reqs, "[digest of ") {
		t.Fatal("digest was not inserted with a clear summary label")
	}
	// The summarizer must have been handed the raw elided tool output, not a marker.
	if !strings.Contains(sm.sawInput, "tool big_tool") {
		t.Fatalf("summarizer input did not carry the elided tool outputs: %q", sm.sawInput)
	}
	// Elide markers must NOT be what the main model sees in the summarized slot — the
	// digest label replaced the earliest one. (Later slots may still be plain markers.)
	if !msgsContain(model.reqs, "SUMMARIZED-DIGEST-SENTINEL") {
		t.Fatal("expected the digest, not only markers")
	}
}

// TestSummarizeFallsBackOnError proves the fail-safe: a summarizer error must not lose
// the investigation — the loop keeps the plain elision markers and still finishes.
func TestSummarizeFallsBackOnError(t *testing.T) {
	sm := &fakeSummarizer{err: errors.New("summarizer 503")}
	li, got := summarizeLoop(t, sm, nil)
	if err := li.Investigate(context.Background(), Request{Title: "summarize fallback"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(got.RootCauses) == 0 {
		t.Fatalf("a summarizer failure must fall back to elision and still finish, got %+v", got)
	}
	if sm.count() == 0 {
		t.Fatal("summarizer should have been attempted")
	}
	model := li.Model.(*scriptModel)
	if msgsContain(model.reqs, "[digest of ") {
		t.Fatal("a failed summarizer must not insert a digest")
	}
	// Plain elision must still have happened (markers present) — otherwise the budget
	// would have hard-killed instead of finishing.
	if !msgsContain(model.reqs, elidedSuffix) {
		t.Fatal("expected plain elision markers after summarizer fallback")
	}
}

// TestSummarizeRefusalAndTruncationFallBack proves a refused or truncated summary is
// also treated as a failure (never a partial/garbage digest inserted).
func TestSummarizeRefusalAndTruncationFallBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp providers.CompletionResponse
	}{
		{"refusal", providers.CompletionResponse{StopReason: "refusal", Text: "I can't"}},
		{"truncated", providers.CompletionResponse{Text: "partial digest cut o", Truncated: true}},
		{"empty", providers.CompletionResponse{Text: "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := &fakeSummarizer{resp: tc.resp}
			li, got := summarizeLoop(t, sm, nil)
			if err := li.Investigate(context.Background(), Request{Title: "summarize " + tc.name}); err != nil {
				t.Fatalf("Investigate: %v", err)
			}
			if len(got.RootCauses) == 0 {
				t.Fatalf("%s summary must fall back and finish, got %+v", tc.name, got)
			}
			if msgsContain(li.Model.(*scriptModel).reqs, "[digest of ") {
				t.Fatalf("%s summary must not be inserted as a digest", tc.name)
			}
		})
	}
}

// sumMetric sums the values of every Prometheus sample line whose name matches.
func sumMetric(body, name string) float64 {
	var total float64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name) {
			continue
		}
		// name{labels} value  — take the last whitespace-separated field.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			total += v
		}
	}
	return total
}

// TestSummarizeBudgetAccountingAndOnePerEvent proves (a) the summarizer's token usage
// is counted into the model-input-token budget accounting, and (b) exactly one
// summarization call happens per compaction event (history_summarizations_total ==
// history_compactions_total == the summarizer's call count).
func TestSummarizeBudgetAccountingAndOnePerEvent(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	// A distinctive input-token count so we can see it land in the counter. The main
	// scriptModel reports zero usage, so any nonzero model_input_tokens is the summarizer's.
	const summTokens = 4242
	sm := &fakeSummarizer{resp: providers.CompletionResponse{
		Text:  digestSentinel,
		Usage: providers.Usage{InputTokens: summTokens},
	}}
	li, got := summarizeLoop(t, sm, m)
	if err := li.Investigate(context.Background(), Request{Title: "summarize accounting"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(got.RootCauses) == 0 {
		t.Fatalf("investigation did not finish: %+v", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	inputTokens := sumMetric(body, "runlore_model_input_tokens_total")
	if inputTokens < summTokens {
		t.Fatalf("summarizer token usage not counted into model_input_tokens_total: got %v, want >= %d", inputTokens, summTokens)
	}
	compactions := sumMetric(body, "runlore_history_compactions_total")
	summarizations := sumMetric(body, "runlore_history_summarizations_total")
	if summarizations != compactions {
		t.Fatalf("expected one summarization per compaction event: summarizations=%v compactions=%v", summarizations, compactions)
	}
	if int(summarizations) != sm.count() {
		t.Fatalf("summarizer calls (%d) must equal summarization events (%v)", sm.count(), summarizations)
	}
	if summarizations < 1 {
		t.Fatal("expected at least one summarization event")
	}
}
