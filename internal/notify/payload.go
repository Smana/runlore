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
//
// The three resource fields say three different things, and only one of them is
// the resource's identity:
//
//   - namespace — the namespace this finding is scoped to: the one the resource is
//     IN, or, for a Namespace object named with no workload inside it, that
//     namespace itself (the one kind whose own identity arrives in this field).
//     Absent when the resource is in no namespace at all — a Node, an RDS
//     instance — where the alert's `namespace` label names whatever exported the
//     series rather than the object. See scopedNamespace.
//   - resource — the resource's name, verbatim, never rewritten.
//   - resource_ref — the whole scoped identity: "namespace/name", or the bare name
//     when there is no namespace to qualify it. The same string the Slack card
//     prints, from the same function (resourceRef), so the two cannot disagree.
//
// Compatibility note (2026-08-18): `namespace` narrowed to the above and
// `resource_ref` was added. Nothing was removed or renamed, so every operator
// template still executes and every JSON consumer still parses — `namespace` is
// `omitempty` and has always been absent on findings that carry no namespace
// (PagerDuty incidents carry none at all). What changed is that it is now absent
// instead of wrong: a template doing "{{.Namespace}}/{{.Resource}}" no longer
// renders "observability/ip-10-11-132-8.ec2.internal" for a Node. Such a template
// should move to "{{.ResourceRef}}", which renders the scope correctly for every
// kind rather than merely stopping short of a false one.
type Payload struct {
	Title            string          `json:"title"`
	Confidence       float64         `json:"confidence"`
	Namespace        string          `json:"namespace,omitempty"`
	Resource         string          `json:"resource,omitempty"`
	ResourceRef      string          `json:"resource_ref,omitempty"`
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
		Namespace: scopedNamespace(inv.Resource), Resource: inv.Resource.Name,
		ResourceRef: resourceRef(inv.Resource),
		CuratedURL:  inv.CuratedURL, Text: Format(inv), Verdict: string(inv.Verdict),
		Severity: inv.Severity, Cluster: inv.Cluster, Environment: inv.Environment,
		Tenant: inv.Tenant, AlertName: inv.AlertName, StartedAt: startedAt,
		Occurrences: inv.Occurrences, PrevCuratedURL: inv.PrevCuratedURL,
		RuledOut: inv.RuledOut, DataGaps: inv.DataGaps,
		Prior: prior, MatchedKnowledge: matched,
	}
}

// scopedNamespace returns the namespace to publish for w: a namespace w is actually
// scoped to, or nothing. Payload.Namespace used to carry providers.Workload.Namespace
// raw, and on an alert-driven investigation that field is the namespace of whatever
// EXPORTED the series — kube-state-metrics, the alerting rule — not the object's. A
// Node is cluster-scoped and an RDS instance is not a Kubernetes object, so
// "namespace": "observability" on either is a place the reader can go and not find
// the resource. Same conflation the Slack card shipped; see resourceRef.
//
// It decides by ASKING resourceRef whether the namespace still contributes to the
// rendered identity — rendering w a second time with the namespace removed and
// comparing — rather than testing the kind itself. That is the whole point of the
// shape: the kind decision (which kinds are cluster-scoped, which are cloud, which
// spellings are not Kubernetes at all, and the one kind whose OWN name arrives in
// the namespace field) lives in resource_scope.go and exists once. A second copy
// here would answer differently the day either list moves, and the card and the
// wire format would disagree about the same investigation.
//
// Asking that way is also what keeps a Namespace object's name — which arrives in
// this very field — from being discarded as a foreign qualifier: resourceRef renders
// it, so it contributes, so it is published. Nothing here knows that kind by name.
//
// Consequently it is fail-safe for exactly the same reason resourceRef is: an
// unknown, unlisted or empty kind still contributes its namespace, so the field is
// unchanged for every resource whose namespace is not KNOWN to be foreign.
func scopedNamespace(w providers.Workload) string {
	if resourceRef(w) == resourceRef(providers.Workload{Kind: w.Kind, Name: w.Name}) {
		return "" // the namespace changed nothing about the identity: it is not w's own
	}
	return w.Namespace
}

// kbUpdateEvent is the value KBUpdatePayload.Event always carries. It exists
// because both deliveries go to the SAME operator-configured URL: an
// investigation and a knowledge-base update are different shapes, and a
// receiver written against Payload would otherwise decode an update into a
// zero-valued investigation and report an empty finding. Payload itself does
// NOT grow this key — its wire format is what external consumers already parse.
const kbUpdateEvent = "kb_update"

// KBUpdatePayload is the delivery payload for a knowledge-base write that
// already landed on the forge — the programmatic counterpart of the chat
// announcement notify's Slack and Matrix notifiers render.
//
// It carries the note at the length it was WRITTEN, with no preview ceiling: a
// receiver here wants the record, and bounding what is RENDERED is a chat
// transport's job (see notify's kbNotePreviewBytes). The fields providers.KBUpdate
// documents as untrusted are untrusted here too, and they leave the network —
// that is what a webhook is for — but the hazard on this path is a malformed or
// injected JSON body rather than chat markup, so encoding/json is the whole
// defence and every value goes out verbatim. A chat escaper applied here would
// corrupt the record instead of protecting it.
type KBUpdatePayload struct {
	Event     string `json:"event"` // always kbUpdateEvent
	Transport string `json:"transport,omitempty"`
	Root      string `json:"root,omitempty"`
	// Channel is the other half of the thread handle, and it ships because
	// "root" alone does not identify a conversation to anything outside this
	// process: a Slack thread_ts is scoped to its channel and a Matrix event id
	// to its room, so a receiver building a permalink back to where the note was
	// typed — which is the use "root" exists for — cannot do it without this.
	// Sending one and not the other made the pair useless while looking complete.
	//
	// providers.KBUpdate.Delivery is deliberately NOT here: it is a routing
	// instruction for the chat sinks, not a fact about the write, and this
	// payload is the record. See TestKBUpdatePayloadCarriesEveryRecordedField for
	// where that decision is written down and enforced.
	Channel string `json:"channel,omitempty"`
	Route   string `json:"route"`
	PR      int    `json:"pr,omitempty"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Author  string `json:"author,omitempty"`
	// ModelDrafted says RunLore's chat model wrote "note" from "author"'s
	// message, rather than "author" having typed it.
	//
	// Sent WITHOUT omitempty, unlike every other optional field here, and that is
	// the point: a receiver storing this as a record has to be able to tell "a
	// human wrote it" from "this producer does not report provenance". With
	// omitempty the two are the same absent key, and the safe reading of an absent
	// key — assume a human wrote it — is exactly the wrong one. The record is
	// three bytes larger and unambiguous.
	ModelDrafted bool   `json:"model_drafted"`
	Note         string `json:"note,omitempty"`
	At           string `json:"at,omitempty"` // RFC3339; "" when unknown
}

// NewKBUpdatePayload maps a KBUpdate to the delivery payload.
func NewKBUpdatePayload(up providers.KBUpdate) KBUpdatePayload {
	at := ""
	if !up.At.IsZero() {
		at = up.At.UTC().Format(time.RFC3339)
	}
	return KBUpdatePayload{
		Event: kbUpdateEvent, Transport: up.Transport, Root: up.Root, Channel: up.Channel,
		Route: string(up.Route), PR: up.PR, URL: up.URL,
		Title: up.Title, Author: up.Author, ModelDrafted: up.ModelDrafted, Note: up.Note, At: at,
	}
}
