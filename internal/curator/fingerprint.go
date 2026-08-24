// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Volatile-identifier patterns. These name a specific host/pod/instance and are
// NOT identity-defining for an incident CLASS — the same problem on a different
// node must dedupe (CORE-681). They are masked out of fingerprints and keys.
var (
	reUUID       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reNodeName   = regexp.MustCompile(`\bip-\d{1,3}-\d{1,3}-\d{1,3}-\d{1,3}(\.[a-z0-9.-]+)?`) // ip-10-11-19-78[.ec2.internal]
	reIPv4       = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	reInstanceID = regexp.MustCompile(`(?i)\b(i|vol|eni|snap|ami)-[0-9a-f]{8,}\b`) // EC2/EBS ids
	reHexHash    = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)                     // dashless uuids / sha blobs
)

// normalizeText masks volatile identifiers (IPs, node hostnames, EC2 ids, long
// hashes/UUIDs) in free text, collapsing them to a single space, so the same
// incident on a different host hashes and BM25-matches alike.
func normalizeText(s string) string {
	s = reUUID.ReplaceAllString(s, " ")
	s = reNodeName.ReplaceAllString(s, " ")
	s = reIPv4.ReplaceAllString(s, " ")
	s = reInstanceID.ReplaceAllString(s, " ")
	s = reHexHash.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// normalizeResourceName reduces a resource name to the identity the curator groups
// by: an AWS ARN collapses to the resource identifier its CloudWatch dimension
// carries, and a trailing pod-name hash is stripped so a per-pod name reduces to its
// controller family. It is a thin alias for providers.NormalizeResourceName, the
// single source of truth shared with the instant-recall path (CORE-681) so the two
// can never drift.
//
// It is deliberately NOT named normalizeWorkloadName any more: providers also
// exports a NormalizeWorkloadName that does only the per-instance-suffix half — pod
// hashes and CronJob run stamps — and a local alias wearing that exact name would
// read as a pass-through to it while meaning something wider. Read providers.NormalizeResourceName for the accepted consequence of the ARN
// collapse — the account and region are dropped, and the surrounding key fields do
// not reliably put them back.
func normalizeResourceName(name string) string {
	return providers.NormalizeResourceName(name)
}

// IncidentKey builds a host-invariant, per-class dedup key for an alert: the alert
// class + affected workload family + cluster, with the volatile pod-hash suffix
// stripped. The same alert on a different pod/node yields the same key (so it
// dedupes to one KB entry), while a different cluster stays distinct. Returned as
// the alert TriggerKey and folded into DupFingerprint. "" when there is no signal.
//
// The AWS account is a segment of its own, appended only when the workload carries
// one. It is what separates two accounts that agree on every other field — a
// CloudWatch-derived rule authors `namespace` as the exporter's, one Prometheus
// stamps one `cluster` label across every account it scrapes, and kind is empty for
// a cloud resource, so "datagrok" in two accounts was ONE key. Via the recurrence
// ledger that key decides suppression, so the collision could drop a genuine
// incident in one account on another account's conclusion.
//
// APPENDED-WHEN-PRESENT, not always: a Kubernetes workload has no account, and
// emitting an empty segment for it would change every K8s key's bytes for nothing.
// Omitting it keeps them byte-identical, and no key is ambiguous — a key with the
// account has six segments, one without has five.
//
// It reads the workload's Account FIELD and never an account embedded in an
// ARN-spelled Name, because a map key is compared by equality and must therefore be
// spelling-invariant: ingestion (providers.ResolveWorkloadIdentity) fills the field
// on BOTH the ARN and the short-name path, so the two spellings of one resource
// still key alike, which is the split this key already exists to close.
//
// ONE-OFF MIGRATION: a cloud resource's key gains a segment, so it no longer matches
// the key the same resource was persisted under in outcome.Ledger's byTrigger index.
// Its recurrence count restarts from zero once, on the first firing after the
// upgrade; the old bucket is simply never looked up again. Kubernetes keys are
// byte-identical (they have no account segment) and are unaffected.
// The same one-time reset now also reaches CronJob-generated workloads:
// NormalizeWorkloadName folds the <unix-minutes> run suffix into the CronJob family,
// so github-teams-sync-29787720 and -29790030 key alike where they used to key apart.
func IncidentKey(alertname string, w providers.Workload, cluster string) string {
	parts := []string{
		strings.TrimSpace(alertname),
		strings.TrimSpace(w.Namespace),
		strings.TrimSpace(w.Kind),
		normalizeResourceName(strings.TrimSpace(w.Name)),
		strings.TrimSpace(cluster),
	}
	if account := strings.TrimSpace(w.Account); account != "" {
		parts = append(parts, account)
	}
	for _, p := range parts {
		if p != "" {
			return strings.ToLower(strings.Join(parts, "|"))
		}
	}
	return ""
}

// stopWords are content-free English filler dropped so reworded causes hash alike.
var stopWords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {}, "in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "of": {}, "by": {}, "with": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {}, "have": {}, "has": {}, "had": {}, "do": {}, "does": {}, "did": {}, "will": {}, "would": {}, "could": {}, "should": {}, "may": {}, "might": {}, "must": {}, "can": {}, "that": {}, "which": {}, "who": {}, "when": {}, "where": {}, "why": {}, "how": {}, "as": {}, "from": {}, "up": {}, "down": {}, "out": {}, "so": {}, "if": {}, "any": {}, "all": {}, "each": {}, "every": {}, "both": {}, "few": {}, "more": {}, "most": {}, "other": {}, "same": {}, "such": {}, "no": {}, "nor": {}, "not": {}, "only": {}, "than": {}, "too": {}, "very": {}, "just": {}, "what": {}, "then": {}, "you": {}, "your": {}, "his": {}, "her": {}, "its": {}, "our": {}, "their": {}, "this": {}, "these": {}, "happened": {}, "happen": {},
}

// Fingerprint builds the dedup query string for a finding: the alert/title, the
// top root-cause signature, and the affected workload. It is a BM25 query (fuzzy),
// not a hash — matched against the catalog index and open-PR titles.
func Fingerprint(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(normalizeText(inv.Title))
	if len(inv.RootCauses) > 0 {
		b.WriteString(" " + normalizeText(inv.RootCauses[0].Summary))
	}
	if len(inv.Changes) > 0 {
		w := inv.Changes[0].Workload
		b.WriteString(" " + w.Namespace + " " + normalizeResourceName(w.Name))
	}
	return strings.TrimSpace(b.String())
}

// Novelty decides whether a finding duplicates an existing catalog entry, by
// scoring its fingerprint against the BM25 catalog index.
type Novelty struct {
	Catalog  catalog.ScoredSearcher // nil → everything is novel (no catalog configured)
	DupScore float64                // top-hit BM25 score ≥ this ⇒ duplicate
}

// Hits returns up to k catalog entries scored against the finding's
// fingerprint — hits[0] drives the duplicate decision, the full slice feeds
// the PR's related-knowledge section (one search, two consumers).
func (n Novelty) Hits(ctx context.Context, inv providers.Investigation, k int) ([]catalog.ScoredEntry, error) { //nolint:revive // ctx kept for future remote-index symmetry
	if n.Catalog == nil {
		return nil, nil
	}
	return n.Catalog.SearchScored(Fingerprint(inv), k)
}

// TopHit returns the highest-scoring catalog entry for a finding's fingerprint.
// ok is false when no catalog is configured or there are no hits. It surfaces the
// score regardless of any threshold, so callers can both observe it and decide.
func (n Novelty) TopHit(ctx context.Context, inv providers.Investigation) (catalog.ScoredEntry, bool, error) {
	hits, err := n.Hits(ctx, inv, 1)
	if err != nil {
		return catalog.ScoredEntry{}, false, err
	}
	if len(hits) == 0 {
		return catalog.ScoredEntry{}, false, nil
	}
	return hits[0], true, nil
}

// DupFingerprint is a deterministic identity for "the same incident", used to
// dedupe curated PRs across re-investigations. It prefers the trigger key stamped
// at trigger time (a structured K8s signal — the alert fingerprint, or the failing
// resource + condition reason): re-investigations of one ongoing incident reword
// the LLM's prose cause but share the same trigger, so keying on the trigger is
// what coalesces them to a single open PR (#137). When no trigger key is present
// (e.g. a human `lore investigate "<symptom>"`), it falls back to the affected-
// resource ref plus the significant-token set of the top root cause. Unlike
// Fingerprint (a fuzzy BM25 query) it is an exact hash. It returns "" when there is
// nothing to key on — an empty fingerprint must never match another.
func DupFingerprint(inv providers.Investigation) string {
	// Strip the volatile pod-hash suffix from the affected resource so the same
	// incident on a different pod/node keys alike (CORE-681). The TriggerKey is
	// already a host-invariant per-class key for alert sources (see IncidentKey).
	res := inv.Resource
	res.Name = normalizeResourceName(res.Name)
	ref := strings.ToLower(res.Ref())
	// The AWS account qualifies the ref, so one instance name in two accounts does not
	// curate as one incident. Appended only when present, so a Kubernetes ref — and
	// therefore its fingerprint — is byte-identical to what it was. Like IncidentKey
	// this reads the Account FIELD and not an account embedded in an ARN-spelled name:
	// inv.Resource is MODEL-WRITTEN free text (submit_findings), so a model that echoes
	// the full ARN back from the seed prompt must still fingerprint as the same
	// incident as one that writes the short name.
	if account := strings.TrimSpace(res.Account); account != "" && ref != "" {
		ref += "@" + strings.ToLower(account)
	}
	if tk := strings.ToLower(strings.TrimSpace(inv.TriggerKey)); tk != "" {
		// "trigger:" namespaces the key so a trigger value can never collide with a
		// prose causeKey from the fallback path below.
		sum := sha256.Sum256([]byte(ref + "|trigger:" + tk))
		return hex.EncodeToString(sum[:])
	}
	cause := ""
	if len(inv.RootCauses) > 0 {
		cause = normalizeText(inv.RootCauses[0].Summary)
	}
	tokens := tokenSet(cause)
	causeKey := strings.Join(tokens, " ")
	if len(tokens) == 0 {
		// The token-set erased the whole cause (all sub-3-char or stopword tokens,
		// e.g. "IO GC" or "db up"). Fall back to the raw lowercased summary so two
		// genuinely different terse/acronym causes on the same resource do not
		// collapse to the same fingerprint and falsely coalesce.
		causeKey = strings.ToLower(strings.TrimSpace(cause))
	}
	if ref == "" && causeKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ref + "|" + causeKey))
	return hex.EncodeToString(sum[:])
}

// tokenSet lowercases s, splits on non-alphanumeric runes, drops generic English
// stopwords and tokens shorter than 3 chars, dedupes, and sorts — an order-independent
// significant-token set so reworded phrasings of one cause normalize to the same key.
func tokenSet(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, ok := stopWords[f]; ok {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
