// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Payload is the exported delivery payload: the single definition of what an
// outbound notification carries. The webhook notifier marshals it as-is; the
// templated notifier exposes it as the template dot. Field set and json tags
// are the webhook notifier's original wire format — do not change tags without
// a compatibility note, external consumers parse them.
type Payload struct {
	Title            string          `json:"title"`
	Confidence       float64         `json:"confidence"`
	Namespace        string          `json:"namespace,omitempty"`
	Resource         string          `json:"resource,omitempty"`
	CuratedURL       string          `json:"curated_url,omitempty"`
	Text             string          `json:"text"`
	Verdict          string          `json:"verdict,omitempty"`
	Severity         string          `json:"severity,omitempty"`
	Cluster          string          `json:"cluster,omitempty"`
	Environment      string          `json:"environment,omitempty"`
	Tenant           string          `json:"tenant,omitempty"`
	AlertName        string          `json:"alert_name,omitempty"`
	StartedAt        string          `json:"started_at,omitempty"` // RFC3339; "" when unknown
	Occurrences      int             `json:"occurrences,omitempty"`
	PrevCuratedURL   string          `json:"prev_curated_url,omitempty"`
	RuledOut         []string        `json:"ruled_out,omitempty"`
	DataGaps         []string        `json:"data_gaps,omitempty"`
	Prior            *PriorPayload   `json:"prior,omitempty"`
	MatchedKnowledge *MatchedPayload `json:"matched_knowledge,omitempty"`
}

// MatchedPayload mirrors providers.MatchedEntry for delivery consumers: the
// pre-existing KB entry this investigation's kb_search matched at clear-match
// strength (distinct from prior, which reports recurrence).
type MatchedPayload struct {
	Path  string  `json:"path,omitempty"`
	Title string  `json:"title,omitempty"`
	URL   string  `json:"url,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// PriorPayload mirrors providers.PriorKnowledge for delivery consumers: what the
// merged KB entry said last time this incident fired.
type PriorPayload struct {
	Cause      string `json:"cause,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	EntryPath  string `json:"entry_path,omitempty"`
	Recalls    int    `json:"recalls,omitempty"`
	Resolved   int    `json:"resolved,omitempty"`
}

// NewPayload maps an (already-redacted) Investigation to the delivery payload.
// Matched knowledge is surfaced only when Prior is nil so the structured field
// never disagrees with the rendered Text (Prior already covers "seen before").
func NewPayload(inv providers.Investigation) Payload {
	startedAt := ""
	if !inv.StartedAt.IsZero() {
		startedAt = inv.StartedAt.UTC().Format(time.RFC3339)
	}
	var prior *PriorPayload
	if p := inv.Prior; p != nil {
		prior = &PriorPayload{Cause: p.Cause, Resolution: p.Resolution, EntryPath: p.EntryPath, Recalls: p.Recalls, Resolved: p.Resolved}
	}
	var matched *MatchedPayload
	if mk := inv.MatchedKnowledge; mk != nil && inv.Prior == nil {
		matched = &MatchedPayload{Path: mk.Path, Title: mk.Title, URL: mk.URL, Score: mk.Score}
	}
	return Payload{
		Title: inv.Title, Confidence: inv.Confidence,
		Namespace: inv.Resource.Namespace, Resource: inv.Resource.Name,
		CuratedURL: inv.CuratedURL, Text: Format(inv), Verdict: string(inv.Verdict),
		Severity: inv.Severity, Cluster: inv.Cluster, Environment: inv.Environment,
		Tenant: inv.Tenant, AlertName: inv.AlertName, StartedAt: startedAt,
		Occurrences: inv.Occurrences, PrevCuratedURL: inv.PrevCuratedURL,
		RuledOut: inv.RuledOut, DataGaps: inv.DataGaps,
		Prior: prior, MatchedKnowledge: matched,
	}
}
