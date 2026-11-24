// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MetricsProvider abstracts VictoriaMetrics/Prometheus (both speak PromQL).
//
// LabelValues is metric/label discovery: it answers "what exists?" so the agent
// never dead-ends on a guessed metric name. A query that matches nothing returns
// an empty result with no hint of the real names a workload exports; LabelValues
// scopes to a matcher + window so it stays cheap on a big TSDB. Metric-name
// discovery uses the label "__name__".
type MetricsProvider interface {
	Query(ctx context.Context, promql string, at time.Time) (Samples, error)
	QueryRange(ctx context.Context, promql string, w TimeWindow, step time.Duration) (Matrix, error)
	// LabelValues lists the values a label takes across the series that match the
	// given matchers (PromQL selectors, e.g. `{namespace="apps"}`), within the
	// window. label "__name__" enumerates metric names. matchers may be empty
	// (whole-TSDB), though callers should scope it so it stays cheap.
	LabelValues(ctx context.Context, label string, matchers []string, w TimeWindow) ([]string, error)
}

// AlertRule is one ALERTING rule as the rules backend defines it: the expression
// that is actually thresholded, plus the metadata that says how it fires.
//
// Query is the load-bearing field. A threshold alert names a specific metric AND a
// specific statistic, and the two are not interchangeable — an alert on
// aws_rds_write_latency_maximum is not corroborated (or refuted) by
// aws_rds_write_latency_average, nor by the read-latency series that happens to
// share its prefix. See AlertRuleReader for the misdiagnoses that motivated this.
type AlertRule struct {
	Name  string        // the alertname the rule fires under
	Query string        // the PromQL/MetricsQL expression the rule thresholds
	For   time.Duration // the `for:` hold-down; 0 means "fires on the first evaluation"
	// State is the rule's current evaluation state as the backend reports it
	// ("firing"/"pending"/"inactive"), or "" when it reports none. It makes a
	// "stale alert" claim checkable instead of assumed.
	State string
	// Health and LastError report whether the rule is EVALUATING at all
	// ("ok"/"err"/"unknown"). A rule in "err" health produces no data, which looks
	// identical to a healthy metric sitting at zero.
	Health    string
	LastError string
	// Group and File locate the rule's definition, so a noisy threshold can be
	// fixed where it lives rather than merely reported.
	Group       string
	File        string
	Labels      map[string]string // rule labels (severity, environment, …)
	Annotations map[string]string // rule annotations (summary, runbook_url, …)
}

// AlertRuleReader is an OPTIONAL capability a MetricsProvider may implement: read
// the ALERTING RULE DEFINITIONS the backend serves. Consumers type-assert for it
// exactly like LogFields/LogStats, so a backend with no rules endpoint (a bare
// remote-read gateway, a query-only proxy) simply omits it and the caller degrades.
// Prometheus and VictoriaMetrics/vmalert both implement it via GET /api/v1/rules.
//
// It exists because an alert-triggered investigation otherwise has to GUESS which
// series the alert thresholded, and guessing wrong inverts the conclusion. Two real
// RDSCriticalLatency investigations did exactly that: the rule thresholds
// aws_rds_*_latency_MAXIMUM, but one read only read_latency_maximum (0-1ms) and
// concluded "false positive", while write_latency_maximum was in fact spiking to
// 63.7ms; the other quoted write_latency_AVERAGE and claimed it "matches the alert
// exactly". The rule text was one request away in both cases.
//
// Implementations return ALERTING rules only (recording rules are not thresholds)
// and MUST be read-only — nothing here creates, edits or silences a rule.
//
// names, when given, is a HINT to scope the read to those alertnames (Prometheus
// and vmalert both scope /api/v1/rules on repeated `rule_name[]` parameters, which
// turns a ~509 KB / 278-rule download into the one rule the caller wants). It is
// best effort in BOTH directions, and callers must treat it as such:
//
//   - the result may contain MORE than names — a backend that does not support the
//     hint simply returns everything, so callers still filter what they got;
//   - the result may contain LESS, up to nothing at all. An EMPTY result from a
//     scoped read is therefore NOT evidence that those alertnames have no rule; it
//     is indistinguishable from a backend that mishandled the hint. A caller that
//     needs to establish absence — or that needs the other alertnames, e.g. to
//     offer a corrected name — MUST re-read with no names before concluding.
//
// An unscoped read (no names) is always authoritative for what the backend serves.
type AlertRuleReader interface {
	AlertRules(ctx context.Context, names ...string) ([]AlertRule, error)
}

// LogsProvider abstracts the logs backend (VictoriaLogs now; Loki etc. later).
type LogsProvider interface {
	Query(ctx context.Context, query string, w TimeWindow) (LogResult, error)
}

// Bucket is one time-bucket of a log-hits histogram: how many lines matched in
// [Time, Time+step). Level is the per-level series label when the backend split
// hits by severity ("" for a single, unsplit series).
type Bucket struct {
	Time  time.Time
	Level string
	Count int64
}

// MsgCount is one dominant log message and its occurrence stats over a window:
// how many lines collapsed to it (after numeric normalization) and the first→last
// span it covered — the "what is flooding the logs" summary.
type MsgCount struct {
	Message string
	Count   int64
	First   time.Time
	Last    time.Time
}

// LogFields is an OPTIONAL discovery capability a LogsProvider may implement:
// the list of field names present in the logs a query matches (with per-field hit
// counts) — the log-side analogue of MetricsProvider.LabelValues. It answers "the
// query returned nothing / the schema I assumed is wrong — what fields do these
// logs ACTUALLY have?" so the agent recovers instead of dead-ending on a guessed
// collector schema. Consumers type-assert for it; VictoriaLogs implements it via
// /select/logsql/field_names.
type LogFields interface {
	// FieldNames returns the field names present in the logs matching query over
	// the window, each with its occurrence count, most-frequent first.
	FieldNames(ctx context.Context, query string, w TimeWindow) ([]FieldCount, error)
}

// FieldCount is one log field name and how many matching lines carried it.
type FieldCount struct {
	Name string
	Hits int64
}

// LogStats is an OPTIONAL analytics capability a LogsProvider may implement:
// error-volume-over-time (Hits) and top-messages-by-count (TopMessages). It is
// separate from LogsProvider so the analytics surface never widens the core
// contract — consumers type-assert for it exactly like GitOpsInspector, and a
// backend that cannot serve analytics (or a future Loki client) simply omits it,
// letting the tool fall back gracefully. VictoriaLogs implements it via
// /select/logsql/hits and a `stats by (_msg)` pipe.
type LogStats interface {
	// Hits returns the match count per step-sized bucket over the window; the
	// backend may split into per-level series (Bucket.Level set) or return a
	// single unsplit series.
	Hits(ctx context.Context, query string, w TimeWindow, step time.Duration) ([]Bucket, error)
	// TopMessages returns up to k dominant messages (numeric tokens collapsed so
	// near-identical lines group), each with its count and first→last span.
	TopMessages(ctx context.Context, query string, w TimeWindow, k int) ([]MsgCount, error)
}

// NetworkProvider abstracts network observability (Hubble now).
type NetworkProvider interface {
	Drops(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}

// CloudChangeFilter narrows which control-plane events CloudChanges keeps. It is a
// parameter of that ONE method rather than a field on Selector, because Selector
// answers "which object" and every other consumer of it — the GitOps providers'
// Changes, the three flow providers' Drops, ResourceHealth — would silently drop a
// filter they never honour. A caller asking to narrow and a callee quietly not
// narrowing is the defect LookupReason exists to prevent one layer up.
//
// The zero value keeps everything, so a caller that does not care says nothing.
type CloudChangeFilter struct {
	// FailedOnly keeps only events the provider recorded as REJECTED calls. Cloud
	// lookups return the most recent MUTATING events, which on a busy cluster are
	// overwhelmingly routine instance and tag churn; a "why did this fail" question
	// wants the rejected calls, and those are usually older than the result cap.
	FailedOnly bool
}

// CloudProvider abstracts read-only cloud-side context for an incident. It adds a
// cloud-layer "what changed" lens (mutating control-plane events — CloudTrail on
// AWS, Cloud Audit Logs on GCP) and cloud resource health (instances/ASGs/nodegroups
// on AWS; the GKE/MIG/Compute equivalents on GCP) that the in-cluster signals can't
// see.
//
// Implemented with native cloud SDKs (aws-sdk-go-v2 on AWS) and in-cluster identity
// (EKS Pod Identity / IRSA on AWS) — not Steampipe and not a bundled CLI (both break
// the single-binary property). Steampipe / cloud MCP servers stay optional MCP
// extensions. Cloud is opt-in (config.cloud.provider).
type CloudProvider interface {
	// CloudChanges returns recent mutating cloud control-plane events (AWS:
	// CloudTrail; GCP: Cloud Audit Logs) in the window, normalized to the
	// engine-agnostic Change model so they join the same "what changed" timeline as
	// GitOps diffs. f narrows WHICH of those events are kept; the zero value keeps
	// all of them.
	CloudChanges(ctx context.Context, sel Selector, w TimeWindow, f CloudChangeFilter) ([]Change, error)
	// ResourceHealth returns cloud-side state/health for resources backing the
	// selector (EC2 instance status, ASG capacity/activities, EKS nodegroup on AWS;
	// analogous GKE/MIG/Compute state on GCP), as normalized lines for the model.
	ResourceHealth(ctx context.Context, sel Selector, w TimeWindow) (LogResult, error)
}

// CloudVocabulary is the cloud-specific vocabulary the cloud tools render their
// model-facing descriptions from. It exists because those descriptions were written
// as AWS literals ("MUTATING AWS control-plane events (CloudTrail)", "EC2 instance-id
// (i-…)"), and a GCP deployment reading them would be told it is querying CloudTrail
// while it queries Cloud Audit Logs.
//
// That is not cosmetic. cloud_tools.go documents a real investigation that dead-ended
// because the model held a wrong belief about how the `resource` argument MATCHES.
// Tool text is the only place that belief comes from, so it has to be true per cloud.
//
// The struct is fragments, not two finished strings, deliberately: the skeleton in
// ChangeDescription/HealthDescription is shared, so the two clouds cannot drift into
// describing structurally different contracts for the same tool. Cloud-independent
// clauses — "OMIT it to see every mutating event, which is the right move when you
// do not know the exact identifier", "and other infra changes invisible to GitOps"
// — live in the skeleton itself, never duplicated into a fragment, so a GCP author
// filling in the cloud-specific nouns cannot accidentally reword or drop one.
//
// Punctuation is part of each fragment's contract, because the skeleton splices
// fragments into sentences rather than terminating each one uniformly: ChangeExamples,
// TimelineExamples, LagNote, InstanceArg and FailureFilterArg are bare phrases with NO
// trailing punctuation; ScopeGuidance ends with ";" (the skeleton appends the OMIT
// clause and its own period); FailureFilterNote and HealthSurface are complete
// sentences ending in "." — or, for FailureFilterNote ONLY, the empty string for a
// cloud with nothing to say, which the renderer omits outright rather than leaving a
// blank sentence.
//
// Not every field feeds ChangeDescription/HealthDescription. InstanceArg and
// FailureFilterArg are JSON-Schema text — CloudResourceHealthTool's `instance_id` and
// CloudWhatChangedTool's `failed_only` argument descriptions — consumed where those
// tools build their schemas, and TimelineExamples belongs to a third tool entirely
// (IncidentTimelineTool), whose sentence fuses three datasources and so is not this
// type's to render. HealthSurface and InstanceArg separately name the same
// optional-argument noun (today, "EC2 instance-id (i-…)" vs "EC2 instance id (i-…)" —
// already spelled two different ways) rather than sharing a constant: they are not
// actually identical text today, so deduplicating would mean either changing one —
// altering rendered output — or defining two near-duplicate constants that dedupe
// nothing. Worth a future cloud's author knowing they are naming the same argument
// twice; the same goes for ChangeExamples and TimelineExamples.
type CloudVocabulary struct {
	Cloud          string // "AWS" | "GCP"
	AuditLog       string // "CloudTrail" | "Cloud Audit Logs"
	ChangeExamples string // bare, comma-joined list of the kinds of change this log carries; no trailing punctuation
	ScopeGuidance  string // how the `resource` argument matches; ends with ";", no period

	// TimelineExamples is ChangeExamples' shorter sibling, for incident_timeline —
	// whose cloud clause is one item in a three-datasource sentence and so names
	// fewer services ("ASG/EC2/EKS/manual actions" against ChangeExamples'
	// "ASG/EC2/EKS/RDS/SG changes, manual actions"). Two fields, not one, because
	// those are not the same text today: collapsing them would reword one of the two
	// shipped tool descriptions, and the promise this whole type exists to keep is
	// that AWS's model-facing text survives the introduction of a second cloud byte
	// for byte.
	TimelineExamples string

	// Engine tags changes and notes this cloud produces, so a note synthesised in the
	// tool layer carries the same engine as the provider's own Change values. It is not
	// rendered into any model-facing text, which is why Validate does not require it:
	// every other field here is a text fragment, and a missing one is visible in the
	// output, whereas a zero Engine is only visible to code.
	Engine Engine

	// FailureFilterNote explains cloud_what_changed's failed_only argument, or is ""
	// for a cloud with nothing equivalent to filter for. It is its own field, rather
	// than folded into ScopeGuidance, because cloud_what_changed's description has
	// already grown one tool-specific paragraph since this struct was sketched (PR
	// #551): a field per paragraph, not just per cloud noun, is what stops the next
	// one vanishing silently the day a tool starts rendering from this struct instead
	// of carrying its own literal.
	FailureFilterNote string

	// FailureFilterArg is the JSON-Schema description of that same failed_only
	// argument — schema text, like InstanceArg, rather than prose ChangeDescription
	// renders. Its AWS nouns are easy to miss because not one of them is a
	// capitalised service name: it calls the incident "a failed AWS write operation",
	// and it promises that "a denied Describe/Get will NOT appear here", Describe and
	// Get being the AWS read verbs. The GCP sentence has to name different verbs
	// (get/list) and rests on a different fact: this tool filters reads out on either
	// cloud, but on GCP the reads are usually not in the log to begin with, because
	// Data Access audit logs are off by default outside BigQuery while Admin Activity
	// logs are always on. A model handed the AWS version would be hunting for the
	// wrong verb names — the same class of wrong belief about how a lookup matches
	// that cloud_tools.go records an investigation dead-ending on.
	FailureFilterArg string

	// WidenedBanner renders the banner shown when a scoped cloud_what_changed lookup
	// matched nothing and the tool retried unscoped. It is a func, not a template
	// string, so the %q verb that formats the resource lives inside a fmt.Sprintf
	// call written in THIS file — a string literal `go vet`'s printf check can
	// actually verify — rather than in a dynamic field threaded into a distant
	// fmt.Fprintf, where a GCP author's typo (%s, or two verbs) would ship
	// %!q(MISSING) to the model with no compiler or vet warning.
	//
	// Being a func is also the one way a fragment can be MISSING rather than merely
	// empty, so nothing reads it directly: RenderWidenedBanner does, and substitutes
	// neutral wording when it is nil. Validate reports a nil one as incomplete.
	//
	// widenedFailedBanner (cloud_tools.go) deliberately has no slot here; the
	// reasoning lives with that constant.
	WidenedBanner func(resource string) string

	LagNote       string // ingestion lag, e.g. "CloudTrail lags ~15m"; bare phrase, no punctuation
	HealthSurface string // what cloud_resource_health actually describes; a complete sentence ending in "."
	InstanceArg   string // schema description for the instance argument; bare phrase, no punctuation

	// HealthWindowNote is the since_minutes clause for cloud_resource_health: what that
	// argument scopes on THIS cloud, and what an unset one means.
	//
	// A fragment rather than skeleton text because the two clouds window different
	// things. AWS scopes ASG *scaling activities*; GCP has no such concept and scopes
	// managed-instance-group *errors*. The skeleton carried AWS's noun, so a GCP model
	// was told it could narrow a lookback over a mechanism its provider does not have —
	// the precise class of wrong belief this whole type exists to prevent, and one the
	// AWS golden structurally cannot catch.
	//
	// Form: a bare clause naming what is scoped, then the unset default in parentheses,
	// ending with "." — it is spliced into HealthDescription.
	HealthWindowNote string

	// HealthWindowArg is the since_minutes SCHEMA description, which is a separate
	// fragment from HealthWindowNote rather than the same string reused.
	//
	// Two fields because the two surfaces are worded differently and always were: the
	// description explains what the argument does in prose, the schema tells the model
	// how to use it in imperative form. Deriving one from the other produced a schema
	// reading "scope the lookback to the last N minutes — scopes the scaling-activity
	// lookback to the incident window (default: recent activities)." — which changed
	// AWS's shipped text for the worse to fix GCP's. Both clouds get their own.
	//
	// Form: an imperative phrase, bare, no punctuation.
	HealthWindowArg string
}

// ChangeDescription renders the cloud_what_changed description for this cloud. It
// joins non-empty sentences rather than splicing every field in positionally, so a
// cloud with FailureFilterNote == "" (nothing to say about a failed-call filter)
// renders one clean space between sentences instead of two.
func (v CloudVocabulary) ChangeDescription() string {
	sentences := []string{
		fmt.Sprintf("List recent MUTATING %s control-plane events (%s) — %s, and other infra changes "+
			"invisible to GitOps.", v.Cloud, v.AuditLog, v.ChangeExamples),
		"Use when no Git change explains the incident.",
		fmt.Sprintf("%s OMIT it to see every mutating event, which is the right move when you do not "+
			"know the exact identifier.", v.ScopeGuidance),
	}
	if v.FailureFilterNote != "" {
		sentences = append(sentences, v.FailureFilterNote)
	}
	sentences = append(sentences, fmt.Sprintf("since_minutes default 90 (%s).", v.LagNote))
	return strings.Join(sentences, " ")
}

// HealthDescription renders the cloud_resource_health description for this cloud.
func (v CloudVocabulary) HealthDescription() string {
	return fmt.Sprintf(
		"Describe %s-side health for the cluster's nodes/capacity: %s Use to confirm a "+
			"node/infra/capacity cause. Optional since_minutes %s",
		v.Cloud, v.HealthSurface, v.HealthWindowNote,
	)
}

// RenderWidenedBanner renders the dropped-scope banner for resource, falling back to
// cloud-neutral wording when a vocabulary leaves WidenedBanner nil.
//
// Read this instead of the field. A nil func is the one way a fragment can be absent
// rather than merely empty, and calling it would panic on the widen path — a branch
// that only runs when a scoped lookup missed and the unscoped retry found something,
// which no smoke test reaches. The panic is recovered a long way up (thread/dispatch),
// so the process survives and the INVESTIGATION is what dies, leaving a log line where
// a root cause should have been. Degraded wording on a rare path beats that trade
// every time.
//
// The fallback drops only the cloud's explanation of WHY the scope missed, which is
// the part that cannot be guessed. What the model must not lose — that its filter was
// dropped and it is now looking at everything — is cloud-independent, so it is stated
// either way.
func (v CloudVocabulary) RenderWidenedBanner(resource string) string {
	if v.WidenedBanner != nil {
		return v.WidenedBanner(resource)
	}
	return fmt.Sprintf("resource %q matched nothing in the window, so the resource filter was "+
		"DROPPED. Showing ALL mutating events in the window instead:\n", resource)
}

// Validate reports every fragment a vocabulary left unset, as a joined error naming
// each field. It returns nil for a complete one.
//
// A cloud provider's constructor test should call this on the vocabulary it returns.
// Nothing calls it at render time on purpose: a half-filled vocabulary is a coding
// mistake, and failing an investigation over one would be a worse outcome than the
// degraded sentence the renderers already produce.
//
// It exists because an empty fragment does not fail — it renders. A vocabulary with
// only Cloud and AuditLog set yields "events (Cloud Audit Logs) — , and other infra
// changes invisible to GitOps", a failed_only argument described as "", and
// "since_minutes default 90 ()". Every one of those reaches a model as confident
// prose. The AWS golden cannot catch it, because AWS is precisely the vocabulary that
// is already complete; the exposure is entirely on clouds added later.
//
// FailureFilterNote is the one omission that is not an error — see its own comment
// for why a cloud may have nothing to say there.
func (v CloudVocabulary) Validate() error {
	required := []struct {
		field  string
		value  string
		render string // what goes wrong when it is empty, in the text a model reads
	}{
		{"Cloud", v.Cloud, "names the cloud in both tool descriptions and all three empty-result messages"},
		{"AuditLog", v.AuditLog, "names the log in cloud_what_changed's description and incident_timeline's cloud clause"},
		{"ChangeExamples", v.ChangeExamples, "lists the kinds of change cloud_what_changed can show"},
		{"TimelineExamples", v.TimelineExamples, "lists the same for incident_timeline's cloud clause"},
		{"ScopeGuidance", v.ScopeGuidance, "tells the model how the resource argument matches — the belief a wrong lookup turns on"},
		{"FailureFilterArg", v.FailureFilterArg, "is cloud_what_changed's failed_only schema description"},
		{"LagNote", v.LagNote, "tells the model how far behind the log runs, inside since_minutes' default"},
		{"HealthSurface", v.HealthSurface, "is the entire subject of cloud_resource_health's description"},
		{"InstanceArg", v.InstanceArg, "is cloud_resource_health's instance_id schema description"},
		{"HealthWindowNote", v.HealthWindowNote, "is cloud_resource_health's since_minutes clause in the " +
			"description — empty leaves the model an argument with no stated effect"},
		{"HealthWindowArg", v.HealthWindowArg, "is cloud_resource_health's since_minutes schema description"},
	}
	var errs []error
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			errs = append(errs, fmt.Errorf("CloudVocabulary.%s is empty; it %s", r.field, r.render))
		}
	}
	if v.WidenedBanner == nil {
		errs = append(errs, errors.New("CloudVocabulary.WidenedBanner is nil; it explains a dropped "+
			"resource scope, and RenderWidenedBanner has to substitute cloud-neutral wording for it"))
	}
	return errors.Join(errs...)
}

// EmptyChangesMessage is what cloud_what_changed returns for a window it found no
// mutating events in.
//
// An empty result is rendered from the vocabulary, not written as a literal in the
// tool, because it is a CLAIM about a named cloud: "no mutating AWS events in the
// window" on a GCP deployment asserts something the tool never looked at. It is also
// the single sentence a model is most likely to copy verbatim into a finding — an
// investigation that closes with "no mutating AWS events in the window" reads as
// evidence to the human on call, and on GCP it would be evidence of nothing.
func (v CloudVocabulary) EmptyChangesMessage() string {
	return fmt.Sprintf("no mutating %s events in the window", v.Cloud)
}

// EmptyFailedChangesMessage is what cloud_what_changed returns when failed_only was
// set and no rejected calls matched. It names the filter that produced the emptiness,
// because "no failures" and "the control plane was quiet" are different claims and
// only one of them was established. See cloud_tools.go's Call for the rest of that
// argument, including why a bounded scan appends its own note instead.
func (v CloudVocabulary) EmptyFailedChangesMessage() string {
	return fmt.Sprintf("no FAILED %s control-plane calls in the window (successful events were not "+
		"listed — re-run without failed_only to see them)", v.Cloud)
}

// EmptyHealthMessage is what cloud_resource_health returns when the provider produced
// no lines. Same reasoning as EmptyChangesMessage: the sentence names a cloud, so the
// cloud has to be the one that was actually queried.
func (v CloudVocabulary) EmptyHealthMessage() string {
	return fmt.Sprintf("no %s resource health returned", v.Cloud)
}

// CloudDescriber is an OPTIONAL CloudProvider extension naming the cloud's own
// vocabulary. Same shape as every other optional capability here (OwnerWalker,
// EventWindower, GitOpsEngineReporter, ProgressNotifier): the tools type-assert for
// it and degrade gracefully.
//
// Degrading means AWSCloudVocabulary, which reproduces the shipped AWS text byte for
// byte — so a provider that does not implement this interface produces exactly the
// tool descriptions it produced before this interface existed.
type CloudDescriber interface {
	CloudVocabulary() CloudVocabulary
}

// CloudPreflighter is an optional CloudProvider extension: one cheap authoritative
// read at startup that confirms the credentials actually grant what the lens needs, so
// a misconfiguration surfaces with a fix attached instead of as a bare 403 partway
// through an investigation.
//
// Optional, and the same shape as every other optional capability here (CloudDescriber,
// OwnerWalker, EventWindower, GitOpsEngineReporter, ProgressNotifier), so the wiring
// runs it for whichever provider offers it rather than one branch of a switch calling a
// concrete method. A provider that cannot probe cheaply simply does not implement it.
//
// A non-nil error must be treated as DIAGNOSTIC, not fatal: see CloudPreflightDenied
// for the distinction the caller has to draw between "this will never work" and "the
// network was briefly unhappy at startup".
type CloudPreflighter interface {
	Preflight(ctx context.Context) error
}

// CloudPreflightDenied reports whether a CloudPreflighter error is an authorization
// failure — the permanent kind, where the tools genuinely cannot work until someone
// changes a binding — rather than a transient one.
//
// The distinction is load-bearing because wiring happens exactly once per process. A
// caller that disables the lens on ANY preflight error turns a 503 or a DNS hiccup
// during a slow pod start into cloud tools that stay unregistered until someone
// restarts the pod, which is strictly worse than the no-probe behaviour it replaced.
// Providers signal the permanent case by wrapping ErrCloudPreflightDenied.
func CloudPreflightDenied(err error) bool { return errors.Is(err, ErrCloudPreflightDenied) }

// ErrCloudPreflightDenied marks a preflight failure as an authorization denial.
// Providers wrap it; callers ask via CloudPreflightDenied.
var ErrCloudPreflightDenied = errors.New("cloud preflight denied")

// ErrEntryUnscreenable says, for certain, that a PR's entry cannot be screened: not one entry, a path
// no writer makes, too large to read, or named by the PR and absent from its head. Any other read
// failure says nothing about the entry and is worth retrying.
var ErrEntryUnscreenable = errors.New("entry cannot be screened")

// AWSCloudVocabulary is the AWS wording, and the fallback for any CloudProvider that
// does not implement CloudDescriber.
//
// Every fragment reaches a model through some rendered surface, and every one of
// those surfaces is pinned byte for byte by
// TestAWSCloudVocabularyStillRendersTheShippedAWSText — a frozen golden captured from
// the tool text as it shipped, compared against the LIVE tool methods rather than
// against this function, so the golden stays an independent witness now that the
// tools render from here.
func AWSCloudVocabulary() CloudVocabulary {
	return CloudVocabulary{
		Engine:           EngineAWS,
		Cloud:            "AWS",
		AuditLog:         "CloudTrail",
		ChangeExamples:   "ASG/EC2/EKS/RDS/SG changes, manual actions",
		TimelineExamples: "ASG/EC2/EKS/manual actions",
		ScopeGuidance: "Optional resource is an EXACT CloudTrail ResourceName — a full ARN, instance-id, " +
			"ASG name, or a resource's full path (e.g. a Secrets Manager secret's \"apps/team/name\") — never a " +
			"service name or substring;",
		FailureFilterNote: "Set failed_only=true when the incident IS a failed AWS operation and you do not " +
			"know which resource it happened to (a failed backup/snapshot job, a rejected API call): results " +
			"are capped at the NEWEST events, which on a busy cluster are routine instance and tag churn, so " +
			"the rejected call you are looking for is usually just past the cap. failed_only spends the cap " +
			"on rejected calls instead and reports each one's error code.",
		FailureFilterArg: "keep only MUTATING control-plane calls that were REJECTED, reporting each " +
			"error code; use when the incident is itself a failed AWS write operation. Read-only calls " +
			"are never listed by this tool, so a denied Describe/Get will NOT appear here",
		WidenedBanner: func(resource string) string {
			return fmt.Sprintf("resource %q matched no CloudTrail events — ResourceName is an exact match "+
				"on the full AWS resource name or ARN (e.g. a secret's full path \"apps/team/name\"), not a "+
				"service or substring. Showing ALL mutating events in the window instead:\n", resource)
		},
		LagNote: "CloudTrail lags ~15m",
		HealthSurface: "EKS nodegroup status + health issues, ASG scaling activities (launch/capacity " +
			"failures), and — when given an EC2 instance-id (i-…) — its instance/system status checks.",
		InstanceArg: "optional EC2 instance id (i-…)",
		HealthWindowNote: "scopes the scaling-activity lookback to the incident window " +
			"(default: recent activities).",
		HealthWindowArg: "scope scaling-activity lookback to the last N minutes",
	}
}

// ---- payloads ----------------------------------------------------------------

// Sample is one instant metric value with its labels.
type Sample struct {
	Metric map[string]string
	Value  float64
	Time   time.Time
}

// Point is a single (time, value) in a range series.
type Point struct {
	Time  time.Time
	Value float64
}

// Series is a labeled time series (range query).
type Series struct {
	Metric map[string]string
	Points []Point
}

// LogLine is one normalized log entry (engine-agnostic).
type LogLine struct {
	Time    time.Time
	Message string
	Fields  map[string]string
}

// changeNoteKind marks a Change that is not an event but a note about the result —
// why the timeline is partial. It is spelled "(truncated)" for compatibility with
// the marker cloud providers already emitted before ChangeNote existed.
const changeNoteKind = "(truncated)"

// ChangeNote builds the sentinel a Change-producing provider appends when the view
// it returns is partial, so the model knows not to read the list as complete. It is
// the Change-shaped counterpart of TruncationLine, and exists here for the same
// reason: the marker is a cross-package contract, and a consumer that has to
// recognise it by matching a string literal has no compiler behind it.
//
// It carries no When, so it sorts and reads as a non-event, and IsChangeNote is the
// only supported way to recognise it.
func ChangeNote(engine Engine, msg string) Change {
	return Change{
		Engine:   engine,
		Type:     ChangeCloudAPI,
		Workload: Workload{Kind: changeNoteKind, Name: msg},
	}
}

// IsChangeNote reports whether c is a ChangeNote rather than a real event. Callers
// that count or render events must skip these; callers that want to surface the
// caveat read c.Workload.Name.
func IsChangeNote(c Change) bool { return c.Workload.Kind == changeNoteKind }

// DefaultCloudMaxEvents bounds how many entries a single cloud lens returns.
//
// ONE constant for every cloud, because the budget is a cross-provider invariant rather
// than a per-provider choice: an investigation's conclusions should not depend on which
// cloud it ran against, and neither cloud should flood a model's context where the other
// would not. Each provider used to declare its own 25 with a comment saying the two must
// match — which is the shape of an invariant with nothing enforcing it.
const DefaultCloudMaxEvents = 25

// ChangeTruncatedNote is the message a Change-producing cloud lens caps with: more
// events matched than were kept, so the answer is among the newest and the search
// should be narrowed.
//
// Shared rather than worded per provider, for the reason ChangeNote and TruncationLine
// are shared: a partial view must read identically on either cloud, and an invariant
// asserted only in a doc comment ("worded exactly as the AWS lens words it") has
// nothing enforcing it. Both provider packages had a byte-identical private copy, one
// typed int and one int64, which is exactly the seam this closes.
func ChangeTruncatedNote(limit int64) string {
	return fmt.Sprintf("results truncated at %d — more events matched; narrow the window or resource", limit)
}

// CallPath renders the "what was called, by whom, and did it work" line both cloud
// audit lenses put in Change.Source.Path.
//
// principal is omitted rather than left dangling when empty: that is not an edge case
// but every Google-initiated system_event and every AWS service-initiated call, and
// "compute.instances.hostError by " reads as a caller whose identity was lost — a
// different and far more alarming claim than "nobody called this".
//
// failCode empty means the call succeeded. When it is set the FAILED suffix is the
// highest-value thing either lens produces: without it a denied or quota-exhausted
// call reads as a change that took effect, and the model reasons forward from a state
// the cloud never reached. Shared so the two clouds cannot spell that suffix
// differently in the same field — KB entries and eval scenarios match on it.
func CallPath(call, principal, failCode, failMessage string) string {
	path := call
	if principal != "" {
		path += " by " + principal
	}
	if failCode == "" {
		return path
	}
	path += " — FAILED: " + failCode
	if failMessage != "" {
		path += " (" + failMessage + ")"
	}
	return path
}

// CloudKind qualifies a cloud provider's own resource-type spelling so downstream
// consumers can tell it from a Kubernetes kind by SHAPE, without knowing any provider.
//
// Workload.Kind is one field carrying two vocabularies: Kubernetes kinds, and whatever
// each cloud calls its resource types. internal/notify has to tell them apart to decide
// whether a namespace qualifier is a fact about the object, and it was doing so by
// accumulating character classes — ':' for AWS's "AWS::EC2::Instance", then '_' for
// GCP's "gke_nodepool". That approach cannot survive a third provider (Azure's
// "Microsoft.Compute/virtualMachines" is dotted and slashed, both shapes that must keep
// reading as Kubernetes), and the '_' rule silently strips the namespace off any real
// namespaced kind a model spells snake_case — "stateful_set", "cron_job".
//
// So the provider, which is the only layer that knows the truth, marks its own: a
// "<engine>::" prefix. ':' is the one character no Kubernetes kind can contain, it is
// already AWS's native separator, and it needs no per-provider knowledge downstream —
// one test, forever. An empty vendorType stays empty rather than becoming a bare
// prefix, since a kind nothing supplied is not a claim about anything.
func CloudKind(engine Engine, vendorType string) string {
	if vendorType == "" {
		return ""
	}
	if strings.Contains(vendorType, ":") {
		// Already colon-qualified (AWS's "AWS::EC2::Instance"). Re-prefixing would
		// change shipped text for no gain — the shape test already answers for it.
		return vendorType
	}
	return string(engine) + "::" + vendorType
}

// TruncationLine is the sentinel appended when a logs/flow query stops at its cap
// with more entries upstream, so the model knows the view is partial. It carries no
// Time or Fields, so it cannot be mistaken for a real entry. Every capping provider
// (Hubble/AWS VPC/GCP firewall flow sources, VictoriaLogs) emits this one line.
func TruncationLine(limit int64) LogLine {
	return LogLine{
		Message: fmt.Sprintf("… results truncated at %d (more matched — narrow the query or shorten the window)", limit),
	}
}

// Samples is an instant-vector result.
type Samples []Sample

// Matrix is a range-query result.
type Matrix []Series

// LogResult is a logs/network query result.
type LogResult []LogLine
