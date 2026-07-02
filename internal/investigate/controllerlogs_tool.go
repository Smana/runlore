package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// fluxControllerNamespace is where Flux controllers run by convention.
const fluxControllerNamespace = "flux-system"

// ControllerLogsTool reads recent logs from a Flux controller, optionally filtered
// to a resource name — surfacing WHY a source/object failed to reconcile (auth,
// reference not found, build error) when resource status alone isn't enough.
type ControllerLogsTool struct {
	Logs providers.LogReader
}

// Name returns the tool name.
func (t ControllerLogsTool) Name() string { return "controller_logs" }

// Description returns the tool description.
func (t ControllerLogsTool) Description() string {
	return "Read recent logs from a Flux controller (source-controller, kustomize-controller, " +
		"helm-controller, notification-controller), optionally filtered to a resource name, over a " +
		"window (default 30m). Use it to learn WHY a source/object failed — e.g. a GitRepository that " +
		"was never created, an auth or checkout error — when gitops_resource_status isn't enough."
}

// Schema returns the JSON schema for the arguments.
func (t ControllerLogsTool) Schema() string {
	return `{"type":"object","properties":` +
		`{"controller":{"type":"string","enum":["source-controller","kustomize-controller","helm-controller","notification-controller"]},` +
		`"resource":{"type":"string","description":"optional: only lines mentioning this name"},` +
		`"since_minutes":{"type":"integer"}},"required":["controller"]}`
}

// Call fetches and renders the controller's recent logs.
func (t ControllerLogsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Controller   string `json:"controller"`
		Resource     string `json:"resource"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if in.Controller == "" {
		return "", fmt.Errorf("controller is required")
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 30
	}
	lines, err := t.Logs.PodLogs(ctx, providers.PodLogQuery{Namespace: fluxControllerNamespace, LabelSelector: "app=" + in.Controller, SinceMinutes: since})
	if err != nil {
		return "", err
	}
	kept := lines[:0:0]
	for _, l := range lines {
		if in.Resource != "" && !strings.Contains(l.Message, in.Resource) {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		return "no matching controller log lines", nil
	}
	var b strings.Builder
	renderLogLines(&b, kept, "more lines")
	return b.String(), nil
}
