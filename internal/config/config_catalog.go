// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/gitrev"
)

// Catalog configures the OKF knowledge catalog read by the agent. Provide either
// a mounted Dir (e.g. a ConfigMap) or a Git repo to sync (which closes the
// read/write loop — the curator's merged PRs flow back into what the agent reads).
type Catalog struct {
	Dir           string         `yaml:"dir"` // OKF bundle path (mounted ConfigMap, or the git-sync mirror)
	Git           CatalogGit     `yaml:"git"`
	Commons       CatalogCommons `yaml:"commons"`
	InstantRecall InstantRecall  `yaml:"instant_recall"`
}

// CatalogCommons is an optional SECOND, read-only catalog root synced from a shared
// upstream repo and indexed alongside the operator's own.
//
// Off by default. Adopting someone else's corpus is a choice, not something that
// should happen on an upgrade — and the entries it carries are generic by design,
// so they must never be mistaken for the platform's own recorded truth. Three
// properties follow from that and are enforced, not documented:
//
//   - every commons entry is MARKED as such wherever it surfaces;
//   - at equal relevance the operator's own entry ranks first;
//   - the curator can never write into the commons root (catalog.WriteRootFor).
//
// What it does NOT do is fire instant recall. Recall applies a structural filter —
// a resource-less entry never agrees with a workload-carrying alert — and commons
// entries are resource-less by construction because they describe a failure class
// rather than one cluster's workload. They ground kb_search, which applies no such
// filter. That is genuine day-one value; claiming recall would not be.
type CatalogCommons struct {
	URL string `yaml:"url"` // shared catalog repo; empty disables the commons root
	// Branch tracks a MOVING branch: whatever upstream pushes there reaches the index
	// at the next interval. Default "main".
	//
	// After ApplyDefaults it carries the effective revision handed to the syncer,
	// which is a tag ref or a commit id when Ref pinned it — a syncer follows exactly
	// one revision and there is no honest way to hand it two.
	Branch string `yaml:"branch"`
	// Ref pins the commons to an IMMUTABLE revision — a tag name (`v1.2.0`, also
	// accepted fully qualified as `refs/tags/v1.2.0`) or a full 40-character commit
	// id — instead of tracking a branch. Mutually exclusive with Branch.
	//
	// The commons is synced from a repository the operator does not own, and its
	// entries reach the model mid-incident through kb_search. Pinning is how an
	// operator reviews what upstream changed before it grounds an investigation, at
	// the cost of not receiving shared fixes until they bump the pin. Both are
	// legitimate trades; tracking a branch stays the default.
	Ref      string   `yaml:"ref"`
	Interval Duration `yaml:"interval"` // re-sync period (default 24h — a shared corpus changes slowly)
	// Dir is where the commons is checked out. It MUST differ from catalog.dir: the
	// two are separate corpora, and sharing a root would let an upstream sync dirty
	// the operator's own checkout.
	Dir string `yaml:"dir"`
	// TokenEnv names an env var holding a read token for a private commons repo.
	TokenEnv string `yaml:"token_env"`
}

// isHex reports whether s is non-empty and made only of hex digits. It backs the
// "did you mean a commit id?" diagnostic below, and so must also fire on the
// abbreviations gitrev.IsObjectID rejects — odd lengths included, which is why it
// is not encoding/hex.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// A `branch` key means "track whatever upstream moves this to", so a pinned
// spelling under one promises an operator the opposite of what it delivers. These
// say what to write INSTEAD; the rule itself is rejectPinnedBranch, stated once.
const (
	commonsBranchRemedy = "use catalog.commons.ref to pin a tag or a commit — branch tracks a moving branch"
	// The operator's OWN catalog shares the syncer with the commons, so a pinned
	// spelling there would work — and that is the problem. Pinning is for a corpus you
	// do not control; this one you do.
	catalogGitBranchRemedy = "your own catalog is deliberately not pinnable, because the curator opens PRs against it and a frozen read root leaves \"merged\" and \"in use\" diverging forever with no signal. Only catalog.commons (a repo you do not control) can be pinned, with `ref`"
)

// rejectPinnedBranch fails when the value of a `branch` key names an immutable
// revision. Neither branch key can honour one today either (both clone
// refs/heads/refs/tags/…), so no configuration that works is broken by catching it
// at load instead of at clone.
func rejectPinnedBranch(key, branch, remedy string) error {
	if !gitrev.IsPinned(branch) {
		return nil
	}
	return fmt.Errorf("%s = %q names an immutable revision: %s", key, branch, remedy)
}

// validateCommonsRevision fails closed on any way of asking for two revisions at
// once, or for a "pin" that is not actually immutable. Every case here would
// otherwise leave an operator believing their corpus is frozen while it moves —
// the precise belief the option exists to make true.
//
// The commons is where the two keys really differ: it has the `ref` escape hatch
// that catalog.git deliberately does not.
func validateCommonsRevision(cc CatalogCommons) error {
	// Both keys set. ApplyDefaults folds a ref into an EMPTY branch only, so the two
	// being non-empty and disagreeing is exactly "the operator wrote both".
	if cc.Ref != "" && cc.Branch != "" && cc.Branch != gitrev.QualifyTag(cc.Ref) {
		return fmt.Errorf("catalog.commons: set either branch (%q, tracks a moving branch) or ref (%q, pins an immutable revision), not both", cc.Branch, cc.Ref)
	}
	switch {
	case cc.Ref == "":
		return rejectPinnedBranch("catalog.commons.branch", cc.Branch, commonsBranchRemedy)
	case strings.HasPrefix(cc.Ref, "refs/") && !gitrev.IsTagRef(cc.Ref):
		// refs/heads/main under `ref` is the dangerous one: the syncer would treat any
		// qualified ref as a pin and never fetch again, silently freezing the commons
		// on whatever the branch pointed at the day it was cloned.
		return fmt.Errorf("catalog.commons.ref = %q is not an immutable revision (only %s… or a full commit id can be pinned); use catalog.commons.branch to track a branch", cc.Ref, gitrev.TagRefPrefix)
	case isHex(cc.Ref) && len(cc.Ref) >= 7 && !gitrev.IsObjectID(cc.Ref):
		return fmt.Errorf("catalog.commons.ref = %q looks like an abbreviated commit id; pin the full 40-character SHA (abbreviations are ambiguous)", cc.Ref)
	}
	return nil
}

// Outcome configures the learning-loop outcome ledger.
type Outcome struct {
	LedgerPath string `yaml:"ledger_path"` // append-only JSONL path (e.g. the git-sync mirror PV); empty disables
	// MaxEvents bounds the JSONL before it is compacted on load (startup / leadership
	// Reload): older paired events are folded into a checkpoint record so the file stops
	// growing forever. A pointer for a three-state knob: nil (key absent) ⇒ a generous
	// default (outcome.DefaultMaxEvents); an explicit 0 ⇒ compaction disabled.
	MaxEvents *int `yaml:"max_events"`
}

// InstantRecall short-circuits the investigation loop when the catalog has a
// high-confidence match for the symptom. Off by default; MinScore is the BM25
// relevance floor (tune for your catalog).
type InstantRecall struct {
	Enabled              bool    `yaml:"enabled"`
	MinScore             float64 `yaml:"min_score"`              // similarity floor for the top hit
	MarginGap            float64 `yaml:"margin_gap"`             // top hit must beat the runner-up by at least this
	SoloFloor            float64 `yaml:"solo_floor"`             // confident bar when there is only one hit (higher than MinScore)
	RequireWorkloadMatch bool    `yaml:"require_workload_match"` // true = exact namespace+workload (also disables scopeless matching); false = namespace-level agreement is enough
	OutcomePrior         float64 `yaml:"outcome_prior"`          // Beta prior strength for outcome decay
	OutcomeFloor         float64 `yaml:"outcome_floor"`          // reject a recall when the outcome factor drops below this

	// StaleAfter down-weights a recall whose last_validated/timestamp is older than
	// this; 0 disables. One 0.75 confidence step, never a rejection — confirm/verify
	// stay the hard gates. An unset horizon plus dateless entries = today's behavior.
	StaleAfter Duration `yaml:"stale_after"`

	// Rerank adds an LLM reranking stage to the recall short-circuit: it ranks the
	// top-K structurally-agreeing candidates against the incident with ONE cheap
	// model call and gates the fire on the reranker's CALIBRATED match confidence
	// (RerankThreshold) instead of the corpus-dependent BM25 magnitude
	// (SoloFloor/MarginGap). This is the principled gate: an enriched real-corpus
	// BM25 score is ~0.1–1.2 (an order of magnitude below the default SoloFloor 4.0),
	// so the magnitude gate only fires where the operator hand-tuned solo_floor to
	// their corpus, whereas a calibrated confidence needs no per-corpus tuning —
	// measured 0/11 → 11/11 fire at default thresholds with perfect precision.
	//
	// Three-state (*bool): **nil (unset) ⇒ ON** whenever instant_recall is enabled,
	// so it works out of the box; explicit `false` disables it (the BM25-magnitude
	// gate is then used, byte-for-byte unchanged); explicit `true` is the same as
	// unset. Use RerankEnabled(). Routes to model.verify (cheaper/faster) when
	// configured, else the main model.
	Rerank          *bool   `yaml:"rerank"`           // nil/true ⇒ reranker ON (default); false ⇒ off (legacy BM25-magnitude gate)
	RerankThreshold float64 `yaml:"rerank_threshold"` // calibrated match-confidence bar to short-circuit (default 0.7; corpus-independent)
	RerankK         int     `yaml:"rerank_k"`         // max structurally-agreeing candidates ranked in one call (default 5; bounded for cost)
	RerankMinScore  float64 `yaml:"rerank_min_score"` // trivial retrieval-score floor below which retrieval found nothing plausible → skip the paid call (cost guard; default 0.1)

	// Hybrid switches recall to fused BM25 + embedding retrieval, gated on COSINE
	// similarity instead of the BM25 score above. Requires model.embeddings to be
	// configured (else recall stays BM25). EXPERIMENTAL — the cosine gate is exercised
	// end-to-end by the hybrid recall eval (internal/investigate/hybrideval_test.go:
	// SearchHybrid + these gates run and NO negative false-fires), but the NUMERIC
	// defaults below are conservative and NOT yet live-measured: a real embedding
	// model's cosine scale is model-specific and must be measured with
	// TestHybridRecallEvalLive, then the thresholds re-derived per model, before
	// graduating out of EXPERIMENTAL. See the graduation criteria in
	// docs/configuration.md and dev/superpowers/plans/2026-07-19-hybrid-recall-eval.md.
	Hybrid          bool    `yaml:"hybrid"`            // enable hybrid (cosine-gated) recall
	HybridMinScore  float64 `yaml:"hybrid_min_score"`  // cosine floor for the top hit (default 0.80)
	HybridMarginGap float64 `yaml:"hybrid_margin_gap"` // cosine margin over the runner-up (default 0.05)

	// VectorCache persists hybrid recall's per-entry embedding cache to disk so
	// a restart or HA failover re-embeds nothing (the in-memory cache already
	// spares unchanged entries WITHIN a process lifetime). Only meaningful when
	// hybrid is on. Default on: persistence only ever helps, and every failure
	// mode (corrupt/stale/missing file) degrades to a cold re-embed.
	VectorCache VectorCache `yaml:"vector_cache"`

	// RerankBackend selects the reranking backend, which answers "which of these
	// candidates, if any, is the runbook for this incident?": "llm" (default, the
	// configured model), "jev" (a decision model answers instead) or "shadow" (both run,
	// the LLM decides, the decider's agreement is recorded). InstantRecall runs only when
	// enabled; this field names which backend runs when it does. Empty defaults to llm
	// in ApplyDefaults.
	RerankBackend string `yaml:"rerank_backend"`
	// RerankThresholdJev is the jev backend's calibrated confidence bar. Required when
	// rerank_backend is jev and decision_model is enabled. Must be in (0,1].
	RerankThresholdJev float64 `yaml:"rerank_threshold_jev"`
}

// RerankEnabled reports whether the instant-recall LLM reranker should run. It is
// ON by default whenever instant_recall is enabled (nil ⇒ on) — the calibrated,
// corpus-independent gate is what makes recall fire out of the box; only an
// explicit `rerank: false` falls back to the legacy BM25-magnitude gate.
func (ir InstantRecall) RerankEnabled() bool {
	return ir.Enabled && (ir.Rerank == nil || *ir.Rerank)
}

// CatalogGit configures periodic Git sync of the catalog into Dir.
type CatalogGit struct {
	URL      string   `yaml:"url"`       // repo to clone/pull; empty disables git-sync
	Branch   string   `yaml:"branch"`    // default "main"
	Interval Duration `yaml:"interval"`  // re-sync period (default 5m)
	TokenEnv string   `yaml:"token_env"` // env var with a read token (empty = anonymous/public)
}

// VectorCache configures on-disk persistence of the hybrid-recall embedding
// cache. Enabled by default (nil/true).
type VectorCache struct {
	Enabled *bool  `yaml:"enabled"`
	Dir     string `yaml:"dir"` // cache dir; "" ⇒ <tmp>/runlore-veccache (ephemeral; point at a PV to persist across restarts)
}

// IsEnabled reports whether cache persistence is on (nil ⇒ default on).
func (v VectorCache) IsEnabled() bool { return v.Enabled == nil || *v.Enabled }

// Curate configures the Phase-2 backlog groomer (lore curate).
type Curate struct {
	StaleAfter          Duration `yaml:"stale_after"`          // close unprotected KB PRs idle longer than this; 0 disables (default 720h)
	RecurrenceThreshold int      `yaml:"recurrence_threshold"` // open a knowledge-gap issue after this many unresolved occurrences of a pattern; 0 ⇒ default 3
	// Retirement opens a human-reviewed "retire" PR for a merged catalog entry whose
	// outcome factor stayed below Floor across at least MinObservations observations.
	// Opt-in: the pass writes to the forge on a schedule, and retirement is a
	// judgment call an operator must consciously enable. Prior/Floor default to the
	// recall gate's outcome_prior/outcome_floor defaults (2.0 / 0.5) so the two
	// gates agree unless deliberately tuned apart.
	Retirement Retirement `yaml:"retirement"`
	// Revalidation is retirement's mirror image: it opens a human-reviewed PR
	// stamping `last_validated` on a merged entry that was recalled for a live
	// incident which then resolved. Opt-in for the same reason as Retirement (it
	// opens PRs against the operator's repo on a schedule), and its Prior/Floor
	// default to the same recall-gate values so an entry can never be proposed
	// for retirement and revalidation at once.
	Revalidation Revalidation `yaml:"revalidation"`
	// Sweeps configures the in-server scheduled grooming loop (leader-only, run by
	// the serve pod). Default mode is dry-run: candidates are logged and audited but
	// no forge write happens until the operator sets mode: apply. mode: off disables
	// the loop entirely (the opt-in CronJob remains the out-of-server alternative).
	Sweeps Sweeps `yaml:"sweeps"`
	// QueueIssue rewrites the Knowledge queue issue on a schedule: the entries waiting for a
	// human decision, the audit sample and the measures. Empty schedule is off; `lore curate
	// queue-issue` rewrites it at once whatever the schedule.
	QueueIssue QueueIssue `yaml:"queue_issue"`
}

// QueueIssue configures the scheduled rewrite of the Knowledge queue issue.
type QueueIssue struct {
	Schedule string `yaml:"schedule"` // "<Day> HH:MM" in UTC ("Mon 08:00"); empty is off
	Slack    bool   `yaml:"slack"`    // post one line linking the issue after each rewrite
}

// Enabled reports whether a schedule is set.
func (q QueueIssue) Enabled() bool { return q.Schedule != "" }

var weekdays = map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday,
	"Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}

// ParseSchedule reads "<Day> HH:MM" (UTC): a three-letter capitalised day, then a zero-padded
// 24-hour time.
func ParseSchedule(s string) (time.Weekday, int, int, error) {
	bad := fmt.Errorf(`curate.queue_issue.schedule must be "<Day> HH:MM" (UTC)`)
	day, clock, ok := strings.Cut(s, " ")
	wd, known := weekdays[day]
	if !ok || !known || len(clock) != 5 || clock[2] != ':' {
		return 0, 0, 0, bad
	}
	h, herr := strconv.Atoi(clock[:2])
	m, merr := strconv.Atoi(clock[3:])
	if herr != nil || merr != nil || clock[0] == '+' || clock[0] == '-' || clock[3] == '+' || clock[3] == '-' || h > 23 || m > 59 {
		return 0, 0, 0, bad
	}
	return wd, h, m, nil
}

// Retirement configures the curate retirement pass (opt-in KB garbage collection).
type Retirement struct {
	Enabled         bool    `yaml:"enabled"`
	MinObservations int     `yaml:"min_observations"` // sustained-decay bar (default 3)
	Floor           float64 `yaml:"floor"`            // retire below this factor (default 0.5)
	Prior           float64 `yaml:"prior"`            // Beta prior strength k (default 2.0)
}

// Revalidation configures the curate revalidation pass (opt-in KB freshness
// confirmation) — the seam that lets `last_validated` be EARNED rather than only
// decay. Set catalog.instant_recall.stale_after well above MinInterval, or the
// pass cannot keep a working entry ahead of the age gate it exists to answer.
type Revalidation struct {
	Enabled bool `yaml:"enabled"`
	// MinInterval is the anti-spam bar: the candidate date must be at least this
	// much newer than the entry's recorded freshness (default 720h — at most one
	// confirmation PR per entry per month). Lower means fresher stamps and more
	// review load.
	MinInterval Duration `yaml:"min_interval"`
	// MaxOpen bounds how many revalidation PRs may await review at once (default
	// 5), counting the ones earlier sweeps left open. It is what keeps the first
	// sweep on a mature catalog from proposing every long-confirmed entry at once.
	//
	// Note that each sweep still costs one forge read per candidate whose entry is
	// already fresh: the anti-spam check reads the file on the base branch, which is
	// where the answer actually lives, so a merged stamp silences the next sweep
	// with no state to keep. Raise curate.sweeps.interval, not MaxOpen, if a large
	// catalog makes that read budget uncomfortable.
	MaxOpen int `yaml:"max_open"`
	// Floor and Prior are the decay calibration, and they must match
	// curate.retirement's: the two passes partition entries by comparing one factor
	// against one floor, so they only stay disjoint while both read the same
	// numbers. Leave them unset to inherit retirement's (falling back to the recall
	// gate's 0.5 / 2.0); setting them to different values is a config error.
	Floor float64 `yaml:"floor"` // revalidate at/above this factor (inherits curate.retirement.floor; else 0.5)
	Prior float64 `yaml:"prior"` // Beta prior strength k (inherits curate.retirement.prior; else 2.0)
}

// Sweep modes: dry-run observes (log + audit, zero forge writes), apply acts,
// off disables the in-server loop. Empty means dry-run — safe by default.
const (
	SweepOff    = "off"
	SweepDryRun = "dry-run"
	SweepApply  = "apply"
)

// Sweeps configures the in-server scheduled grooming sweeps.
type Sweeps struct {
	Mode     string   `yaml:"mode"`     // "" ⇒ dry-run | "apply" | "off"
	Interval Duration `yaml:"interval"` // default 6h; the first sweep waits one full interval
}

// Enabled reports whether the in-server sweep loop should run at all.
func (s Sweeps) Enabled() bool { return s.Mode != SweepOff }

// DryRun reports whether sweeps must not write to the forge (the default posture).
func (s Sweeps) DryRun() bool { return s.Mode == "" || s.Mode == SweepDryRun }

// Forge holds git-forge authentication and the curation target repo.
type Forge struct {
	// Provider selects the forge backend: "github" (default, empty ⇒ github) or
	// "gitlab". Only one of GitHubApp / GitLab is meaningful at a time, matching
	// whichever provider is selected.
	Provider      string    `yaml:"provider"`
	GitHubApp     GitHubApp `yaml:"github_app"`
	GitLab        GitLab    `yaml:"gitlab"`
	KBRepo        string    `yaml:"kb_repo"`        // GitHub: "owner/name"; GitLab: the project path ("group/project", possibly nested)
	BaseBranch    string    `yaml:"base_branch"`    // PR/MR target branch (default "main")
	GitHubAPIURL  string    `yaml:"github_api_url"` // override for GHES/tests (default https://api.github.com)
	DupScore      float64   `yaml:"dup_score"`      // file-time catalog BM25 dedup threshold (default 5.0)
	MinConfidence float64   `yaml:"min_confidence"` // file-time quality gate: min overall confidence (default 0.75)
	// SkipVerdicts lists investigation verdicts that must NOT draft a KB PR — the
	// finding is still delivered to chat, but no repo artifact is created. Values are
	// validated against the verdict enum (no_action|action_suggested|action_required|
	// inconclusive). Empty (default) draws no distinction: every verdict is eligible,
	// preserving pre-gate behaviour. Recommended production value: ["no_action"].
	SkipVerdicts []string `yaml:"skip_verdicts"`
	// GitHost names the host the forge serves GIT REMOTES from — the ONE host
	// RunLore's forge credential may be sent to when it clones (what_changed's
	// GitOps repos, source_diff's allowlisted repos). Empty (the default) derives
	// it: github_api_url's host for GitHub (api.github.com ⇒ github.com),
	// gitlab.base_url's host for GitLab.
	//
	// It exists for GitHub Enterprise with SUBDOMAIN ISOLATION, the one shape the
	// derivation cannot resolve: the API is served from api.HOSTNAME while git and
	// web stay on HOSTNAME, so an API URL of "https://api.ghe.example.com" says
	// nothing about which of the two hosts a clone should authenticate to.
	// Guessing is not an option in either direction — guess api.HOSTNAME and the
	// credential is withheld from every GitOps repo (an empty what_changed on
	// every investigation, visible only as a data-gaps line, RunLore #495); guess
	// HOSTNAME and a deployment that really does serve git from api.HOSTNAME sends
	// its token to a host it is not valid for. Validate therefore REFUSES an
	// "api."-prefixed API host (other than api.github.com) unless this names the
	// git host, so the misconfiguration is loud at startup instead of silent
	// forever. A bare host: no scheme, path, port, userinfo, and ASCII only.
	GitHost string `yaml:"git_host"`

	// DedupBackend selects the file-time dedup gate: "bm25" (default, the DupScore
	// threshold below) or "jev" (a decision model answers "same incident pattern?" —
	// asked only for a top BM25 hit at or above the related-entries floor of 0.2, below
	// which the bm25 gate decides as it always did).
	DedupBackend string `yaml:"dedup_backend"`
	// DedupSkipAbove / DedupAnnotateAbove are the jev backend's band edges. Above skip:
	// do not file, record a confirmation. Between: file, naming the suspect in the PR/MR
	// description (never in the entry, which is what gets committed — see
	// providers.KBEntry.SuspectedDuplicate). Below annotate: file as today. Both are
	// REQUIRED when DedupBackend is jev — a
	// default nobody measured would be invented rather than chosen.
	DedupSkipAbove     float64 `yaml:"dedup_skip_above"`
	DedupAnnotateAbove float64 `yaml:"dedup_annotate_above"`
}

// GitLab holds GitLab forge credentials. Unlike GitHubApp (a short-lived,
// minted installation token), GitLab has no GitHub-App equivalent bot
// identity: auth is a static project or group access token (scope: api),
// referenced by env-var indirection only — the config stores the env var
// NAME, never the token value — and sent as the PRIVATE-TOKEN header.
type GitLab struct {
	BaseURL  string `yaml:"base_url"`  // self-managed instance root, e.g. https://gitlab.example.com; empty = gitlab.com
	TokenEnv string `yaml:"token_env"` // env var holding the project/group access token
}

// GitHubApp holds GitHub App credentials. The private key mints 1-hour
// installation tokens (no long-lived PAT); it is referenced from a Secret, never
// inlined. Required permissions: contents:read (diff), and on the KB repo
// issues:write + pull_requests:write + contents:write (curation).
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyRef  string `yaml:"private_key_ref"` // Secret name/key (e.g. via External Secrets)
	PrivateKeyEnv  string `yaml:"private_key_env"` // v1: env var holding the PEM private key
}
