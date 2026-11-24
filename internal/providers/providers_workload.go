// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"regexp"
	"strings"
	"unicode"
)

// Workload identifies a Kubernetes object, or — when it names a cloud resource —
// that resource plus the cloud scope it lives in.
//
// Account and Region are that scope. They exist because Kind/Name/Namespace do not
// separate two AWS accounts: a CloudWatch-derived alert rule authors `namespace` as
// a literal (the exporter's), one Prometheus stamps a single `cluster` external
// label across every account it scrapes, and Kind is empty for a cloud resource. So
// an RDS instance named "datagrok" in two accounts was ONE identity everywhere a key
// is built from these fields. They are stamped at ingestion (ResolveWorkloadIdentity)
// from the alert's account/region labels AND from an ARN-spelled name, so they are
// present regardless of which spelling the alert used — which is what lets an
// identity key both fuse the two spellings and split the two accounts.
//
// Both are EMPTY for a Kubernetes object, whose identity is namespace/name and
// nothing else. Nothing renders them: Ref() is unchanged, so a K8s workload's ref,
// its entry frontmatter and its keys are byte-identical to what they were before
// these fields existed.
type Workload struct {
	Kind      string
	Name      string
	Namespace string
	Region    string // AWS region / GCP location; "" for a Kubernetes object and for an unqualified resource
	Account   string // AWS account id / GCP project id; "" for a Kubernetes object and for an unqualified resource

	// Scope is what the cluster's own discovery said about Kind, when anything did.
	// Populated where a discovery answer is reachable (see KindScoper) and left
	// ScopeUnknown everywhere else, which is every consumer's cue to keep doing what
	// it did before rather than to assume cluster-scoped.
	//
	// Deliberately NOT part of the workload's identity: Ref(), the recall index and
	// the dedup fingerprint all read Kind/Name/Namespace only, so the same object
	// scoped and unscoped is still one object.
	Scope ResourceScope
}

// Ref renders the workload as "namespace/name", or just "namespace" when the
// name is unknown (common for alert-triggered investigations), or "" when the
// namespace is unknown too. It is the canonical form used for structural recall
// matching, curated-entry resources, and outcome-ledger attribution.
func (w Workload) Ref() string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}

// EntryResourceRef narrows a rendered workload ref to a single, whitespace-free
// value that RunLore's own merge gate accepts in a knowledge-base entry's
// `resource:` frontmatter — kbvalidate rejects any resource containing " \t\r\n"
// outright — and that recall's structural filter can actually match.
//
// It exists because Ref() renders whatever is in Workload.Name, and on a curated
// or captured finding that is MODEL-WRITTEN free text: submit_findings fills
// affected_resource (internal/investigate/tools.go), and a finding covering
// several objects routinely arrives as a list — "essentials, monitoring,
// argocd-app-of-apps" — which Ref() renders as "argocd/essentials, monitoring,
// argocd-app-of-apps". Written verbatim, that value fails the gate, so the entry's
// pull request can never be merged. On the thread-capture path that is the worst
// possible shape of failure: the human is told the write succeeded and the
// announcement fires, because both are 1:1 with the LANDED FORGE WRITE, which did
// land — only the merge is impossible, and nothing surfaces it.
//
// It used to promise that whitespace-free input was returned EXACTLY as given, so
// that no entry which merges today would be written differently. #518 retired that
// promise, because clearing the merge gate is not the bar that matters:
//
//	resource: argocd/essentials,monitoring,argocd-app-of-apps
//
// That live draft carries no whitespace, so it PASSED the gate and merged — and
// recall compares `resource` by STRING EQUALITY (investigate.resourceAgrees,
// forgiving only a pod-hash suffix), so no incident's workload ref is ever that
// string. The entry was unrecallable the moment it landed, with nothing to surface
// it: a silent death, strictly worse than the loud CI rejection a whitespace-bearing
// sibling got. Passing the gate while being permanently unmatchable is the failure
// this function now exists to prevent, not just the reverse.
//
// The rewrite is therefore keyed on what a MATCH could ever have used. A Kubernetes
// namespace and name are RFC 1123 labels/subdomains: lowercase alphanumerics, "-"
// and "." only. So "," ";" "(" "[" "{" cannot appear in either half of a ref that
// matches anything, and a trailing "/" leaves an empty name half that Ref() can
// never render. Cutting at the first of those, and trimming that slash, can only
// turn a value that could never match into one that can. Anything a match could
// have used is still returned untouched — dots, digits and pod-hash suffixes
// included.
//
// Narrowing, not dropping: `resource` is the structural-recall index, and a
// NON-EMPTY resource also disables recall's scopeless tier, so a wrong value is
// worse than none. The first listed object is the one the namespace was rendered
// against and is a real object, so keeping it preserves the index. Nothing is
// lost — the full list still reaches the entry BODY verbatim (draftKBEntry's
// "Affected resource" line and thread.ConceptEntry's Context section both render
// the un-narrowed ref).
//
// It is idempotent, which is what lets the draft path re-inspect an
// already-narrowed value to decide whether to warn about it.
//
// KNOWN CONSEQUENCE: two different multi-object findings that happen to lead with
// the same object now write the SAME `resource`, while curator.DupFingerprint
// keeps them distinct (it hashes the full, un-narrowed Ref()). So the frontmatter
// index can collide where the dedup identity does not — a new class, and stated
// here rather than left to be discovered. It is strictly better than what it
// replaces, where the entry either could not be merged at all or could never be
// recalled, and the entry body still distinguishes them for a reader.
func EntryResourceRef(ref string) string {
	fields := strings.Fields(ref)
	if len(fields) == 0 {
		return ""
	}
	// The leading field only: qualifying detail the model appended as prose
	// ("… (cluster=shared, instance i-0fd8…)") belongs in the body, not in an index key.
	tok := fields[0]
	// Then cut at the first character no matching ref can contain — a list separator
	// or an opening bracket glued to the name without a space.
	if i := strings.IndexAny(tok, ",;([{"); i >= 0 {
		tok = tok[:i]
	}
	// A trailing "/" would leave a ref with an empty name half.
	return strings.TrimRight(tok, "/")
}

// reDeployPod matches a volatile pod-name suffix: a Deployment pod is
// <name>-<rs-hash>-<pod-hash>, e.g. "harbor-registry-59598dbd57-ltkzw". The suffix
// names one ephemeral pod, not the controller family it belongs to.
var reDeployPod = regexp.MustCompile(`-[a-f0-9]{8,10}-[a-z0-9]{5}$`) // <name>-<rs-hash>-<pod-hash>

// reJobRun matches a CronJob run suffix: the Job a CronJob creates is named
// <cronjob>-<unix-minutes>, e.g. "github-teams-sync-aqemia-29787720". The suffix
// names one scheduled RUN, not the CronJob family it belongs to. The optional
// second group is the completion index a completionMode: Indexed Job adds to its
// pods (<cronjob>-<unix-minutes>-<index>), which the 8-digit stamp in front of it
// anchors — without that anchor an ordinal tail is indistinguishable from a
// StatefulSet's and must never be stripped.
//
// The floor of 8 stops the short numeric tails that appear all over real workload
// names from collapsing — StatefulSet ordinals (vmagent-vmagent-0), cluster
// ordinals (shared-0), RDS instance suffixes (…-instance-1) and IP-derived node
// names (ip-10-20-0-144) are 1–3 digits, and merging those would fold genuinely
// distinct workloads into one incident.
//
// The ceiling of 9 stops the LONGER all-digit tails that are not timestamps. A
// 12-digit AWS account id is the common one: `${name}-${account_id}` is a standard
// Terraform naming convention, and ParseResourceID feeds this function, so an
// unbounded rule made acme-logs-111111111111 and acme-logs-222222222222 — two
// buckets in two different accounts — one identity, contradicting the very reason
// Workload.Account exists. A unix-minute stamp is 8 digits until 2160; 9 is
// headroom, not a guess.
//
// The rule cannot separate a stamp from an 8-digit YYYYMMDD date suffix, so a
// resource named <name>-20260824 does collapse to <name>. That is accepted: a
// date-stamped name is per-run by construction too, which is the case this exists
// to fold.
var reJobRun = regexp.MustCompile(`-[0-9]{8,9}(?:-[0-9]{1,3})?$`) // <cronjob>-<unix-minutes>[-<index>]

// minFamilyName is the shortest remainder a strip may leave. Below it the "family"
// is not a name any more: a legacy EC2 id like i-12345678 is `i` plus 8 digits, and
// stripping it yielded "i" — which then compared equal to every other such id, so
// two unrelated instances became one incident identity.
const minFamilyName = 2

// NormalizeWorkloadName strips a trailing per-instance suffix so a name reduces to
// its controller family: a Deployment pod (<name>-<rs-hash>-<pod-hash>), a
// DaemonSet/StatefulSet-revision pod (<name>-<5-char hash containing a digit>) and
// a CronJob run Job (<name>-<unix-minutes>, and the <name>-<unix-minutes>-<index>
// pod of an Indexed one) all collapse to <name>. The rules run to a FIXED POINT, so
// a CronJob's pod sheds both its hash and its run stamp. Names without
// such a suffix are returned unchanged, so real trailing words (e.g. "redis-cache")
// and short numeric tails (e.g. "vmagent-vmagent-0") are preserved — a StatefulSet
// replica ordinal is folded only where the Kind is known to be a Pod's (see
// NormalizePodName and Workload.IdentityName). It is idempotent.
//
// The CronJob case was the expensive omission. A CronJob that fails once emits a
// Job named for THAT run, and the run number reaches four separate consumers — the
// alert TriggerKey, the suppression gate's recurrence chain, DupFingerprint and the
// recall gate — so each new run presented as a brand-new incident: a fault already
// investigated five times restarted at occurrence #1, recall could not match the
// entry written for the previous run, and the curator filed a duplicate KB PR per
// run. Collapsing to the CronJob family repairs all four at once.
//
// This is the single source of truth for per-instance-suffix normalization — pod
// hashes and CronJob run stamps. It is shared by the curator dedup path (curator.DupFingerprint / IncidentKey — CORE-681, so the
// same incident on a different pod dedupes to one KB entry) AND the instant-recall
// structural gate (investigate.resourceAgrees), so a pod-scoped alert carrying the
// volatile hash still matches the normalized workload stored on a KB entry. Homed
// here — not in curator — because both packages already import providers, which
// owns the Workload type; investigate must not import curator (no cycle).
func NormalizeWorkloadName(name string) string {
	// Running the rules in sequence returned <cronjob>-<stamp> for a CronJob pod, so
	// an entry stored under the family name never matched an incoming pod-scoped
	// alert — the recall miss this function exists to prevent, reintroduced by the
	// shape of the fix. Terminates because every step strictly shortens the name.
	for {
		next := stripInstanceSuffix(name)
		if next == name {
			return name
		}
		name = next
	}
}

// NormalizePodName is NormalizeWorkloadName for a name KNOWN to be a Pod's: after
// the per-instance rules it also folds ONE StatefulSet replica ordinal
// (<statefulset>-<ordinal>) into the StatefulSet family, so vmagent-vmagent-0, -1
// and -2 are one workload the way three pods of a Deployment already are (#513).
//
// It is a separate function, not another rule in NormalizeWorkloadName, because
// only the Kind licenses the fold: on a name of unknown kind a short numeric tail is
// ordinary naming — aurora-serverless-postgres-old-1 and -2 are two databases, and
// ip-10-20-0-144 is a node — and folding it would fuse distinct resources in the
// dedup key and the suppression chain. Callers that hold a Workload should read
// Workload.IdentityName, which consults the Kind; this is the string-level rule.
//
// ONE ordinal, not a fixed point: a StatefulSet named sts-0 has pods sts-0-N, and
// folding to a fixed point would erase the StatefulSet's own name. So this is NOT
// idempotent on such a family — Workload.AgreesWithEntryName reads a stored entry
// both as written and folded once for exactly that reason.
func NormalizePodName(name string) string {
	return stripReplicaOrdinal(NormalizeWorkloadName(name))
}

// stripReplicaOrdinal removes ONE trailing -<digits> segment, or returns its input
// unchanged. It runs after the per-instance rules, so the tail it sees on a real pod
// name is a StatefulSet ordinal: a hash or run stamp has already been folded.
func stripReplicaOrdinal(name string) string {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || !isOrdinal(name[i+1:]) {
		return name
	}
	// The same debris guard as stripInstanceSuffix: a strip that leaves too little to
	// be a name is not a family.
	family := strings.TrimRight(name[:i], "-")
	if len(family) < minFamilyName {
		return name
	}
	return family
}

// isOrdinal reports whether s is a non-empty run of ASCII digits.
func isOrdinal(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// stripInstanceSuffix removes at most ONE trailing per-instance suffix, or returns
// its input unchanged. NormalizeWorkloadName drives it to a fixed point.
func stripInstanceSuffix(name string) string {
	// Each rule binds its match ONCE. Spelling this as a switch on MatchString and
	// then re-deriving the match with FindString reads well and runs the regexp
	// engine twice per hit — measured 1.47x slower on pod names than binding it here.
	var cut int
	if m := reDeployPod.FindString(name); m != "" {
		cut = len(name) - len(m)
	} else if i := strings.LastIndexByte(name, '-'); i >= 0 && isPodHashTail(name[i+1:]) {
		cut = i
	} else if m := jobRunSuffix(name); m != "" {
		cut = len(name) - len(m)
	} else {
		return name
	}
	// A strip that leaves a trailing hyphen (name--12345678) or too little to be a
	// name is not a family, it is debris — and debris compares equal to other debris.
	// Leave the name alone rather than invent an identity for it.
	family := strings.TrimRight(name[:cut], "-")
	if len(family) < minFamilyName {
		return name
	}
	return family
}

// jobRunSuffix returns the trailing run suffix, or "". The one-byte test in front is
// what keeps the common path cheap: reJobRun can only match a name whose last byte is
// a digit, and almost no workload name ends in one, so this skips the regexp engine
// entirely for them — measured 183ns/name to 140ns/name over a realistic mix.
func jobRunSuffix(name string) string {
	if n := len(name); n == 0 || name[n-1] < '0' || name[n-1] > '9' {
		return ""
	}
	return reJobRun.FindString(name)
}

// isPodHashTail reports whether suf is the 5-char alphanumeric hash a DaemonSet or
// StatefulSet-revision pod carries. The digit requirement is what separates it from a
// real trailing word like the "cache" in "redis-cache".
func isPodHashTail(suf string) bool {
	return len(suf) == 5 && strings.ContainsAny(suf, "0123456789") && isAlnum(suf)
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
