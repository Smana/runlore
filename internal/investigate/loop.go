package investigate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/providers"
)

const systemPrompt = `You are an SRE incident investigator. The cause is unknown — investigate by
calling the available tools to gather evidence (start with what_changed), reason about both
change-caused and no-change causes, then call submit_findings exactly once with ranked root causes,
evidence, and anything you could not determine. Be honest about uncertainty.`

// LoopInvestigator is the ReAct investigation loop: it drives a ModelProvider with
// tools, feeds tool results back, and finishes when the model calls submit_findings
// (or MaxSteps is reached). The completed investigation is handed to OnComplete.
type LoopInvestigator struct {
	Model      providers.ModelProvider
	Tools      []Tool
	Log        *slog.Logger
	MaxSteps   int
	OnComplete func(providers.Investigation) // delivery hook (Slack/Matrix later)
}

// Investigate runs the loop for a request. It implements Investigator.
func (li *LoopInvestigator) Investigate(ctx context.Context, req Request) error {
	byName := map[string]Tool{}
	specs := make([]providers.ToolSpec, 0, len(li.Tools)+1)
	for _, t := range li.Tools {
		byName[t.Name()] = t
		specs = append(specs, providers.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	specs = append(specs, submitFindingsSpec())

	messages := []providers.Message{{Role: "user", Content: seedPrompt(req)}}
	maxSteps := li.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}

	for step := 0; step < maxSteps; step++ {
		resp, err := li.Model.Complete(ctx, providers.CompletionRequest{System: systemPrompt, Messages: messages, Tools: specs})
		if err != nil {
			return fmt.Errorf("model: %w", err)
		}
		if len(resp.ToolCalls) == 0 {
			li.Log.Warn("investigation inconclusive (no submit_findings)", "title", req.Title)
			return nil
		}
		messages = append(messages, providers.Message{Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			if tc.Name == submitFindingsName {
				inv, perr := parseFindings(tc.Args)
				if perr != nil {
					messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: "error: " + perr.Error()})
					continue
				}
				li.deliver(req, inv)
				return nil
			}
			messages = append(messages, providers.Message{Role: "tool", ToolCallID: tc.ID, Content: li.runTool(ctx, byName, tc)})
		}
	}
	li.Log.Warn("investigation hit max steps", "title", req.Title, "max", maxSteps)
	return nil
}

func (li *LoopInvestigator) runTool(ctx context.Context, byName map[string]Tool, tc providers.ToolCall) string {
	tool, ok := byName[tc.Name]
	if !ok {
		return "unknown tool: " + tc.Name
	}
	out, err := tool.Call(ctx, tc.Args)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

func (li *LoopInvestigator) deliver(req Request, inv providers.Investigation) {
	li.Log.Info("investigation complete",
		"title", req.Title, "confidence", inv.Confidence,
		"root_causes", len(inv.RootCauses), "unresolved", len(inv.Unresolved))
	if li.OnComplete != nil {
		li.OnComplete(inv)
	}
}

func seedPrompt(req Request) string {
	return fmt.Sprintf("Incident: %s (source=%s). Workload: %s/%s. Reason: %s. Message: %s.\nInvestigate the likely cause.",
		req.Title, req.Source, req.Workload.Namespace, req.Workload.Name, req.Reason, req.Message)
}
