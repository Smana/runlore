// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestCompletionResponseRefused locks in the refusal classification: a safety/policy
// stop reason (case-insensitive) reports Refused()==true; a normal termination
// (end_turn/stop/length/max_tokens) or an empty reason reports false.
func TestCompletionResponseRefused(t *testing.T) {
	refused := []string{
		"refusal", "content_filter", "safety", "prohibited_content", "blocklist", "spii",
		"Refusal", "CONTENT_FILTER", "Safety", "SPII", // case-insensitive
	}
	for _, sr := range refused {
		if !(providers.CompletionResponse{StopReason: sr}).Refused() {
			t.Errorf("StopReason %q should report Refused()==true", sr)
		}
	}
	notRefused := []string{"end_turn", "stop", "max_tokens", "length", "MAX_TOKENS", "tool_use", ""}
	for _, sr := range notRefused {
		if (providers.CompletionResponse{StopReason: sr}).Refused() {
			t.Errorf("StopReason %q should report Refused()==false", sr)
		}
	}
}

// TestVerdictConclusive pins the ONE definition of "this verdict is an answer":
// the three actionability verdicts conclude, inconclusive does not, and neither
// does an unset or unknown value (a pre-verdict ledger event, or a model reply the
// parser could not normalize).
func TestVerdictConclusive(t *testing.T) {
	for _, v := range []providers.Verdict{providers.VerdictNoAction, providers.VerdictActionSuggested, providers.VerdictActionRequired} {
		if !v.Conclusive() {
			t.Errorf("verdict %q should report Conclusive()==true", v)
		}
	}
	for _, v := range []providers.Verdict{providers.VerdictInconclusive, "", "maybe", "NO_ACTION"} {
		if v.Conclusive() {
			t.Errorf("verdict %q should report Conclusive()==false", v)
		}
	}
}

// TestNormalizeWorkloadName pins the shared pod-hash normalization now homed in the
// providers package (both curator dedup and instant-recall matching call it). The
// boundary is the safe one the curator tests already established: strip a
// Deployment <rs-hash>-<pod-hash> and a DaemonSet/StatefulSet 5-char hash, but
// never a legitimate trailing word like "redis-cache". It must be idempotent.
func TestNormalizeWorkloadName(t *testing.T) {
	cases := map[string]string{
		"node-exporter-prometheus-node-exporter-km6ld": "node-exporter-prometheus-node-exporter", // DaemonSet pod hash
		"web-7d9c8b6f5-abcde":                          "web",                                    // Deployment <rs-hash>-<pod-hash>
		"harbor-registry-59598dbd57-ltkzw":             "harbor-registry",                        // the live pod-scoped alert
		"node-exporter-prometheus-node-exporter":       "node-exporter-prometheus-node-exporter", // controller name, unchanged
		"redis-cache":                                  "redis-cache",                            // 5-char tail but no digit → kept
		"web":                                          "web",
		"":                                             "",

		// CronJob-generated Job names: <cronjob>-<unix-minutes>. Without this every
		// failed run keys as its own incident, splitting the recurrence chain, the
		// dedup fingerprint and the recall gate once per run.
		"github-teams-sync-aqemia-29787720":      "github-teams-sync-aqemia",
		"github-teams-sync-aqemia-mdft-29787720": "github-teams-sync-aqemia-mdft",
		"github-teams-sync-aqemia-29790030":      "github-teams-sync-aqemia", // a later run collapses to the same family
		"wet-collab-data-ingestion-29791885":     "wet-collab-data-ingestion",

		// The suffix has to be long AND all-digit. Short numeric tails are ordinary
		// naming (cluster ordinals, StatefulSet replicas, IP-derived node names) and
		// collapsing them would merge genuinely distinct workloads.
		"vmagent-vmagent-0":                "vmagent-vmagent-0",
		"aurora-serverless-postgres-old-1": "aurora-serverless-postgres-old-1",
		"ip-10-20-0-144":                   "ip-10-20-0-144",
		"datagrok-group-sync-manual-j8g":   "datagrok-group-sync-manual-j8g", // one-off Job, not a run suffix
		"job-1234567":                      "job-1234567",                    // 7 digits — below the run-suffix floor

		// The POD of a CronJob's Job carries both a run stamp and a hash. Stripping one
		// rule per call left the stamp behind, so an entry stored under the family name
		// never matched a pod-scoped alert — the recall miss this change exists to fix.
		// Spelled with a realistic 5-char hash, which is what caught it.
		"github-teams-sync-aqemia-mdft8-29787720":   "github-teams-sync-aqemia",
		"github-teams-sync-aqemia-29787720-3-mdft8": "github-teams-sync-aqemia", // completionMode: Indexed
		"github-teams-sync-aqemia-29787720-3":       "github-teams-sync-aqemia", // its Job, un-podded

		// Long all-digit tails that are NOT timestamps must survive. `${name}-${account_id}`
		// is a standard Terraform convention and ParseResourceID feeds this function, so
		// collapsing 12 digits made two buckets in two accounts one identity.
		"acme-logs-111111111111": "acme-logs-111111111111",
		"acme-logs-222222222222": "acme-logs-222222222222",

		// A strip must never leave debris, because debris compares equal to other debris.
		// A legacy EC2 id is one letter plus 8 digits; stripping gave "i", and every such
		// id then agreed with every other.
		"i-12345678":    "i-12345678",
		"-12345678":     "-12345678",
		"foo--12345678": "foo", // the empty segment goes with the stamp, not into the name
	}
	for in, want := range cases {
		if got := providers.NormalizeWorkloadName(in); got != want {
			t.Errorf("NormalizeWorkloadName(%q) = %q, want %q", in, got, want)
		}
		// Idempotency: a second pass must be a no-op (the recall gate normalizes both
		// sides, sometimes an already-normalized value).
		if got := providers.NormalizeWorkloadName(want); got != want {
			t.Errorf("NormalizeWorkloadName not idempotent for %q: %q", want, got)
		}
	}
}

// TestUsageTotal pins the one rule every consumer of a Usage needs and each was
// previously re-deriving inline: the billable token count is InputTokens +
// OutputTokens, with neither cache field added a second time. Both are strict
// SUBSETS of InputTokens after normalization — the Anthropic client, the only
// one reporting a cache write, sets InputTokens to input + cache_read +
// cache_creation — so adding either again would double-count.
func TestUsageTotal(t *testing.T) {
	u := providers.Usage{InputTokens: 150, OutputTokens: 7, CachedInputTokens: 100, CacheWriteTokens: 20}
	if got := u.Total(); got != 157 {
		t.Fatalf("Total() = %d, want 157 — the cache fields are already inside InputTokens and must not be counted twice", got)
	}
	if got := (providers.Usage{}).Total(); got != 0 {
		t.Fatalf("an unreported (zero) Usage totals %d, want 0", got)
	}
}

// TestEstimateTokensCountsEveryWireSurface pins that EstimateTokens counts each
// part of a request that actually travels: the system prompt, every tool spec's
// name + description + schema, message content, and the assistant tool-call
// JSON. Counting only m.Content is the specific under-estimate this function
// exists to avoid, so each surface is added one at a time and the estimate must
// grow every time.
func TestEstimateTokensCountsEveryWireSurface(t *testing.T) {
	if got := providers.EstimateTokens("", nil, nil); got != 0 {
		t.Fatalf("empty request: got %d, want 0", got)
	}
	// 4 chars/token, exact: "sys!" (4) + "hello world!" (12) = 16 chars = 4.
	msgs := []providers.Message{{Role: "user", Content: "hello world!"}}
	if got := providers.EstimateTokens("sys!", msgs, nil); got != 4 {
		t.Fatalf("system + content: got %d, want 4", got)
	}

	withToolCall := []providers.Message{
		msgs[0],
		{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "t", Args: `{"a":"bbbb"}`}}},
	}
	baseline := providers.EstimateTokens("sys!", withToolCall, nil)
	if baseline <= 4 {
		t.Fatalf("assistant tool-call Args go over the wire and must be counted: got %d, want more than 4", baseline)
	}

	tools := []providers.ToolSpec{{Name: "t", Description: "does a thing", Schema: `{"type":"object"}`}}
	if got := providers.EstimateTokens("sys!", withToolCall, tools); got <= baseline {
		t.Fatalf("tool specs are re-sent on every request and must be counted: got %d, want more than %d", got, baseline)
	}
}

func TestFingerprintMarkerRoundTrip(t *testing.T) {
	const fp = "abc123def456"
	body := "Drafted by RunLore — x\n\n" + providers.FingerprintMarker(fp)
	if got := providers.ParseFingerprintMarker(body); got != fp {
		t.Fatalf("round-trip: want %q, got %q", fp, got)
	}
	if providers.FingerprintMarker("") != "" {
		t.Fatal("empty fingerprint must render an empty marker")
	}
	if got := providers.ParseFingerprintMarker("no marker here"); got != "" {
		t.Fatalf("absent marker must parse to empty, got %q", got)
	}
}

// costOnlyKeeps is the whitelist CostOnly's doc comment declares: the fields
// that describe what the exchange COST, as opposed to what it answered. It is
// stated here rather than derived from the method, so a field moving into or
// out of the whitelist has to be a deliberate edit to this line.
var costOnlyKeeps = map[string]bool{"Usage": true, "Attempts": true}

// TestCompletionResponseCostOnlyKeepsOnlyTheCost pins the whitelist CostOnly
// is. Every model client returns out.CostOnly() alongside an error, and that is
// the only thing standing between a caller and a half-decoded Text or an
// unterminated tool call from a failed stream — content that is not an answer
// and must never be read as one, while the tokens the provider already reported
// are real and billed.
//
// It reflects over the struct rather than listing the fields: a CostOnly
// written as a whitelist is only as good as its author remembering to revisit
// it, and a hand-written list in the test would need the same remembering. The
// fixture-completeness check below is what makes the reflection bite — a field
// added to CompletionResponse and left out of the fixture fails here, which
// forces the author to classify it as cost or as reply.
func TestCompletionResponseCostOnlyKeepsOnlyTheCost(t *testing.T) {
	full := providers.CompletionResponse{
		Text:      "half a sentence before the stream br",
		ToolCalls: []providers.ToolCall{{ID: "tc-1", Name: "get_logs", Args: `{"ns":"apps"`}},
		Usage: providers.Usage{
			InputTokens: 1200, OutputTokens: 340,
			CachedInputTokens: 900, CacheWriteTokens: 64,
		},
		Truncated:  true,
		StopReason: "max_tokens",
		Opaque:     json.RawMessage(`[{"type":"thinking","signature":"sig"}]`),
		Attempts:   3,
	}

	in := reflect.ValueOf(full)
	rt := in.Type()
	for i := range rt.NumField() {
		if !rt.Field(i).IsExported() {
			t.Fatalf("%s is unexported — this test cannot read it, and CostOnly's whitelist would go unchecked", rt.Field(i).Name)
		}
		if in.Field(i).IsZero() {
			t.Fatalf("fixture leaves %s at its zero value, so this test cannot tell whether CostOnly kept or dropped it — "+
				"populate it above and decide whether it belongs in costOnlyKeeps", rt.Field(i).Name)
		}
	}

	out := reflect.ValueOf(full.CostOnly())
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		got, want := out.Field(i).Interface(), in.Field(i).Interface()
		if costOnlyKeeps[name] {
			if !reflect.DeepEqual(got, want) {
				t.Errorf("CostOnly() dropped %s: got %v, want %v — the provider already billed it", name, got, want)
			}
			continue
		}
		if !out.Field(i).IsZero() {
			t.Errorf("CostOnly() kept %s = %v; it describes a reply that never arrived and must not reach a caller", name, got)
		}
	}
}

// deliverOnly implements providers.Notifier and NOTHING else — the shape every
// existing sink (webhook, templated) has. It is what the optionality of the
// KB-update capability is measured against.
type deliverOnly struct{}

func (deliverOnly) Deliver(context.Context, providers.Investigation) error { return nil }

// TestKBUpdateNotifierIsOptional pins that announcing a knowledge-base write is
// an OPTIONAL capability, not a widening of Notifier. Folding DeliverKBUpdate
// into Notifier would force every existing sink to grow a method it has no use
// for, and would turn "this sink does not announce KB writes" from a capability
// check in Multi into a compile error across the repo.
func TestKBUpdateNotifierIsOptional(t *testing.T) {
	var n providers.Notifier = deliverOnly{}
	if _, ok := n.(providers.KBUpdateNotifier); ok {
		t.Fatal("a plain Notifier must NOT satisfy KBUpdateNotifier — the capability is opt-in")
	}
	if got := reflect.TypeOf((*providers.Notifier)(nil)).Elem().NumMethod(); got != 1 {
		t.Fatalf("Notifier has %d methods, want 1 (Deliver) — the KB-update capability must not widen it", got)
	}
	// The capability itself must be exactly the one delivery method: embedding
	// Notifier (as ThreadNotifier does) would make it non-optional again for any
	// sink that wants only the announcement.
	if got := reflect.TypeOf((*providers.KBUpdateNotifier)(nil)).Elem().NumMethod(); got != 1 {
		t.Fatalf("KBUpdateNotifier has %d methods, want exactly 1 (DeliverKBUpdate)", got)
	}
}

// kbUpdateUntrusted / kbUpdateTrusted classify EVERY KBUpdate field by whether a
// notifier must escape it before rendering it into a chat message. The set is
// stated here rather than derived, so a field added to KBUpdate has to be
// deliberately classified — the announcement is a new egress for model-authored
// text, and an unclassified field is one nobody decided to escape.
//
// Untrusted: Note/Title/Author are model- or human-authored (redacted upstream,
// still not RunLore's words); URL, Root and Channel are returned by the forge and
// the chat transport respectively, which is why the thread reply already wraps the
// URL in thread.Untrusted.
//
// Channel is untrusted for Root's reason and needs the classification for a
// second one: it is ADDRESSING as well as data. A sink that renders it must
// escape it like any transport-reported string, and no announcement does today —
// it is used to address a threaded delivery, which is not rendering.
//
// Delivery is trusted because RunLore sets it from its own configuration; it is
// never reported by a chat system and never rendered at all.
//
// ModelDrafted is trusted for the same reason and one more: it is a boolean, so
// there is nothing in it to escape. Being unescapable is NOT the same as being
// safe to ignore — a renderer that drops it attributes model prose to a named
// human — which is why the announcement surfaces carry their own tests that the
// two provenance routes cannot render identically. This classification answers
// only "must a notifier escape it", and the honest answer is no.
var (
	kbUpdateUntrusted = map[string]bool{"Title": true, "Author": true, "Note": true, "URL": true, "Root": true, "Channel": true}
	kbUpdateTrusted   = map[string]bool{"Transport": true, "Route": true, "PR": true, "At": true, "Delivery": true, "ModelDrafted": true}
)

// TestKBUpdateClassifiesEveryFieldForEscaping makes the two maps above bite: a
// new KBUpdate field that appears in neither (or in both) fails here, forcing
// its author to decide whether a notifier must escape it.
func TestKBUpdateClassifiesEveryFieldForEscaping(t *testing.T) {
	rt := reflect.TypeOf(providers.KBUpdate{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			t.Fatalf("KBUpdate.%s is unexported — a notifier cannot render it, and this classification would go unchecked", f.Name)
		}
		if kbUpdateUntrusted[f.Name] == kbUpdateTrusted[f.Name] {
			t.Errorf("KBUpdate.%s is in neither or both of kbUpdateUntrusted/kbUpdateTrusted — "+
				"decide whether a notifier must escape it before rendering it", f.Name)
		}
	}
	for _, m := range []map[string]bool{kbUpdateUntrusted, kbUpdateTrusted} {
		for name := range m {
			if _, ok := rt.FieldByName(name); !ok {
				t.Errorf("KBUpdate has no field %q — stale classification entry", name)
			}
		}
	}
}

// TestEntryResourceRefNarrowsToTheMergeGatesShape pins the one property the
// knowledge base's merge gate cares about — kbvalidate rejects any `resource:`
// containing whitespace — and the one property recall cares about: the value
// that survives is still a usable structural index, not an empty string.
//
// The whitespace-bearing fixtures are the live one from #491: a finding covering
// three Argo CD Applications reached Workload.Name as the model's own
// comma-and-space list, so Ref() rendered "argocd/essentials, monitoring,
// argocd-app-of-apps" and the entry's own validate job rejected it.
//
// The list and bracket fixtures are the two live drafts from #518. Recall matches
// `resource` by STRING EQUALITY (investigate.resourceAgrees), so a value carrying
// a character a Kubernetes namespace or name cannot contain is not merely untidy —
// it can never match anything, and the entry ships dead.
func TestEntryResourceRefNarrowsToTheMergeGatesShape(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"model listed several objects, comma-and-space", "argocd/essentials, monitoring, argocd-app-of-apps", "argocd/essentials"},
		{"a plain ref is untouched", "tooling/harbor-registry", "tooling/harbor-registry"},
		{"a bare namespace is untouched", "tooling", "tooling"},
		{"a dotted node name keeps every dot", "observability/ip-10-11-189-250.ec2.internal", "observability/ip-10-11-189-250.ec2.internal"},
		{"a pod-hash suffix is not this function's business", "tooling/harbor-registry-59598dbd57-ltkzw", "tooling/harbor-registry-59598dbd57-ltkzw"},
		{"empty stays empty", "", ""},
		{"whitespace-only yields empty rather than a blank resource", "   \t ", ""},
		// A comma-joined list clears the whitespace gate, which is exactly why it is
		// the worse of the two live failures: the PR merged and the entry can never
		// match. No Kubernetes namespace or name contains a comma or a semicolon, so
		// cutting at the first one loses nothing a match could have used.
		{"a comma-joined list without whitespace is cut to its first object", "argocd/a,b,c", "argocd/a"},
		{"a trailing comma goes with the list separator", "argocd/a,b,c,", "argocd/a"},
		{"a semicolon separates a list too", "a;b;", "a"},
		// A parenthetical qualifier appended WITHOUT a space survives the field split,
		// so it needs its own cut: "(", "[" and "{" are all invalid in a k8s name.
		{"a parenthetical glued to the name is dropped", "observability/ip-10-11-189-250.ec2.internal(cluster=shared, instance i-0fd8c3c351590a3a0)", "observability/ip-10-11-189-250.ec2.internal"},
		{"a bracketed qualifier likewise", "argocd/app[prod]", "argocd/app"},
		{"a braced qualifier likewise", "argocd/app{prod}", "argocd/app"},
		{"a trailing slash leaves an empty name half, so it goes", "argocd/app/", "argocd/app"},
		{"surrounding whitespace is trimmed", "  tooling/harbor  ", "tooling/harbor"},
		{"a tab separator counts as whitespace too", "tooling/harbor\tregistry", "tooling/harbor"},
		{"a dangling slash would leave an empty name half", "tooling/ harbor", "tooling"},
		{"a value that is nothing but a qualifier yields empty, not garbage", "(cluster=shared)", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := providers.EntryResourceRef(tt.ref)
			if got != tt.want {
				t.Errorf("EntryResourceRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
			if strings.ContainsAny(got, " \t\r\n") {
				t.Errorf("EntryResourceRef(%q) = %q — still contains whitespace, which the merge gate rejects outright", tt.ref, got)
			}
			if again := providers.EntryResourceRef(got); again != got {
				t.Errorf("EntryResourceRef is not idempotent: %q → %q → %q", tt.ref, got, again)
			}
		})
	}
}
