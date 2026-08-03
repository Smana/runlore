// SPDX-License-Identifier: Apache-2.0

// Package eval replays recorded incident cases through the investigation loop and
// scores whether the agent identifies the root cause — a reproducible RCA benchmark
// (cf. ITBench). A case records the evidence each tool returns, so the eval measures
// the model+loop's reasoning over fixed evidence, independent of a live cluster.
package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Case is one replayable incident.
type Case struct {
	Name string `yaml:"name"`
	// AlertTitle is the incident title a REAL trigger would carry (e.g. the
	// Alertmanager alertname). Optional; DisplayName falls back to Name.
	//
	// It matters because Name is a file-ish slug and the loop uses DisplayName as the
	// Request title. A full investigation overwrites that with a title the model
	// writes, so the slug never shows — but INSTANT RECALL short-circuits before the
	// model runs, so it delivers a card titled "harbor-chart-bump" where production
	// would read "HarborProbeFailure". Same reason recall's query is weaker: the
	// slug is most of what buildRecallQuery gets.
	AlertTitle string            `yaml:"alert_title,omitempty"`
	Prompt     string            `yaml:"prompt"` // the incident description (seeds the loop)
	Tools      map[string]string `yaml:"tools"`  // tool name -> recorded evidence the tool returns
	Expected   Expected          `yaml:"expected"`
	// GroundTruth is optional live-scenario ground truth carried into replay. When
	// present it unlocks the richer scoring the model-comparison benchmark reports:
	// data-source coverage (expected_sources) and blind LLM-judge rubric grading
	// (root_cause / expected_action). Absent ⇒ keyword-only scoring, as before.
	GroundTruth *GroundTruth `yaml:"ground_truth,omitempty"`

	// Workload is the incident's affected workload (namespace + name). It seeds the
	// request and — when a catalog fixture is present — drives the recall structural
	// gate (resource agreement). Optional; zero for alerts without a workload.
	Workload *CaseWorkload `yaml:"workload,omitempty"`
	// CatalogDir, when set, points at a directory of knowledge-base markdown entries
	// RELATIVE to the case file. Its presence seeds an instant-recall catalog for this
	// case and wires Recall + the adversarial verify pass into the replay loop exactly
	// as production does — so the closed recall→verify loop is exercised mechanically in
	// the replay eval. Absent ⇒ the case replays with no recall, unchanged.
	CatalogDir string `yaml:"catalog_dir,omitempty"`
	// CommonsDir, when set, points at a directory of knowledge-base markdown entries
	// RELATIVE to the case file, loaded as the KNOWLEDGE COMMONS — a second, read-only
	// root indexed alongside the operator's own (catalog.SetCommonsDir).
	//
	// It is deliberately NOT CatalogDir with a different name. Entries loaded through
	// CatalogDir come back with Entry.Commons == false: they are the operator's own
	// knowledge, and instant recall may answer an incident from them. Pointing
	// CatalogDir at a commons snapshot would therefore replay an ordinary local
	// catalog and quietly contradict the one guarantee the commons makes.
	//
	// Setting it also wires kb_search into the replay loop, because that is the ONLY
	// route by which a commons entry can ever reach the model (recall refuses it by
	// provenance). A case without this field keeps its previous tool surface exactly.
	CommonsDir string `yaml:"commons_dir,omitempty"`
	// Recall optionally tunes the recall gates for this case (mirrors config
	// instant_recall). Absent (or a zero field) ⇒ the production default. Consulted only
	// when a knowledge fixture is present (CatalogDir and/or CommonsDir).
	Recall *CaseRecall `yaml:"recall,omitempty"`
	// ExpectRecall asserts the recall outcome mechanically and fails the case when unmet:
	//   short_circuit     — recall fired and its answer was delivered (loop skipped)
	//   withdrawn         — recall fired, the verify pass REVIEWED the entry and
	//                       rejected it, and the loop fell through to a full
	//                       investigation
	//   verify_unavailable — recall fired but the verify pass could not run at all
	//                        (model error, or a response with no usable verdict); the
	//                        fail-closed gate forced the same fall-through as an
	//                        outright rejection, but for a different reason — a case
	//                        asserting "withdrawn" does NOT pass on this outcome, so a
	//                        flapping model endpoint surfaces as a failure rather than
	//                        a green run that means something else
	//   fired             — recall fired (short_circuit, withdrawn, or
	//                       verify_unavailable)
	//   rejected          — a recall gate rejected the hit: recall never fired
	// Empty ⇒ no recall assertion (existing cases are unaffected).
	ExpectRecall string `yaml:"expect_recall,omitempty"`

	// Gate controls whether this case counts toward the nightly -fail-under threshold.
	// Absent ⇒ true: every case gates unless it opts out, so nothing changes by
	// accident. `gate: false` marks a case that exists to MEASURE rather than to
	// assert.
	//
	// The distinction is not cosmetic. A -fail-under 0.7 campaign tolerates exactly one
	// missed case whatever the case count (3/4 and 5/6 both clear it; 2/4 and 4/6 both
	// do not), so a case whose failure is the intended FINDING would spend the whole
	// budget and leave a genuine regression elsewhere with nowhere to show up. Worse,
	// CI would report the measurement as the regression. A measurement case still runs,
	// still scores and still appears in the published scorecard — it just does not vote
	// on whether the nightly is red.
	//
	// A pointer so an absent field is distinguishable from an explicit `gate: false`;
	// see Case.gates.
	Gate *bool `yaml:"gate,omitempty"`

	// dir is the directory the case file was loaded from, used to resolve CatalogDir
	// and CommonsDir. Set by Load; unexported so YAML never populates it.
	dir string
}

// CaseWorkload is a case's affected workload for the request + recall structural gate.
type CaseWorkload struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

// CaseRecall tunes the recall gates for a replay case. A zero field takes the same
// production default as config.InstantRecall, so a case need only override what it
// must (e.g. a low solo_floor so a single-entry fixture fires deterministically).
type CaseRecall struct {
	MinScore             float64 `yaml:"min_score"`
	MarginGap            float64 `yaml:"margin_gap"`
	SoloFloor            float64 `yaml:"solo_floor"`
	RequireWorkloadMatch bool    `yaml:"require_workload_match"`
	OutcomePrior         float64 `yaml:"outcome_prior"`
	OutcomeFloor         float64 `yaml:"outcome_floor"`
}

// Expected is the RCA scoring spec for a case.
//
// MustContain and RootCauseEntities are NOT two independent gates: Score matches both
// over the same haystack (the claim — see claimText) with the same case-insensitive
// substring rule, so a term listed in both is simply required twice. The only
// behavioural difference is that a non-empty RootCauseEntities switches ON the
// Distractors over-claim penalty. Keep the split editorial — mechanism words in
// MustContain, blamed entities in RootCauseEntities — and don't duplicate across them.
type Expected struct {
	MustContain       []string `yaml:"must_contain"`        // keywords that must appear in the claim (recall, over claim text)
	MinConfidence     float64  `yaml:"min_confidence"`      // confidence floor (0 = no floor)
	RootCauseEntities []string `yaml:"root_cause_entities"` // entities that MUST be named as the cause; same match as MustContain, and its presence is what enables Distractors
	Distractors       []string `yaml:"distractors"`         // entities present in the case's own evidence that a correct claim has no reason to name — not even to dismiss, since claimText excludes ruled_out; only evaluated when root_cause_entities is non-empty
}

// Load reads every *.yaml / *.yml case in dir.
func Load(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: name comes from reading the operator-supplied cases dir
		if err != nil {
			return nil, err
		}
		var c Case
		if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if c.Name == "" {
			c.Name = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		c.dir = dir // resolve CatalogDir/CommonsDir relative to the case file's directory
		cases = append(cases, c)
	}
	return cases, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// hasCatalog reports whether the case ships any knowledge fixture — its own catalog,
// a commons root, or both. It is what arms instant recall + the verify pass for the
// replay, so a commons-only case is still recall-exercising (recall is consulted and
// must REFUSE the commons entry; see Case.CommonsDir).
func (c Case) hasCatalog() bool { return c.CatalogDir != "" || c.CommonsDir != "" }

// gates reports whether this case votes on the nightly -fail-under gate. Absent
// (the default) counts as true, so a case only ever leaves the gate by saying so.
func (c Case) gates() bool { return c.Gate == nil || *c.Gate }

// buildCatalog loads the case's knowledge fixtures into a catalog wired the way
// production is.
//
// Without a commons root this is exactly the previous one-liner, so the shipped
// catalog_dir cases replay unchanged. With one it goes through
// catalog.NewWithCommons, which is where the "commons root set BEFORE the load"
// ordering that stamps Entry.Commons is stated.
//
// A commons-only case gets a fresh EMPTY directory as the operator's own root, which
// is not a workaround but the scenario itself: a deployment that has curated nothing
// yet is the state the commons exists to cover. It is removed as soon as the reload
// has read it — entries and the index live in memory from then on — so the catalog
// owns nothing on disk and the caller has no lifetime to manage.
func (c Case) buildCatalog(ctx context.Context) (*catalog.Catalog, error) {
	own := filepath.Join(c.dir, c.CatalogDir)
	if c.CommonsDir == "" {
		return catalog.New(own)
	}
	commons := filepath.Join(c.dir, c.CommonsDir)
	// An unreadable commons root is FATAL here, and only here. ReloadContext warns and
	// continues when the commons fails to load, which is right in production — the
	// operator's own entries are what an incident depends on and must not go down with
	// a flaky upstream — and exactly wrong for a measurement. A mistyped or moved path
	// would silently rebuild this case as a second CONTROL: a treatment arm with an
	// empty corpus, publishing a delta of zero that is indistinguishable from the
	// honest finding the case files tell readers to trust.
	fi, err := os.Stat(commons)
	if err != nil {
		return nil, fmt.Errorf("commons root: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("commons root %s is not a directory", commons)
	}
	if c.CatalogDir == "" {
		d, mkErr := os.MkdirTemp("", "runlore-eval-own-")
		if mkErr != nil {
			return nil, fmt.Errorf("empty own-catalog root: %w", mkErr)
		}
		defer func() { _ = os.RemoveAll(d) }()
		own = d
	}
	cat, err := catalog.NewWithCommons(ctx, own, commons)
	if err != nil {
		// Named for the OWN root deliberately: the reload only ever returns an error for
		// that root — a commons Load failure is warned and swallowed inside it, and the
		// stat above is what catches the case that matters — so blaming the commons here
		// would send a reader to the wrong directory.
		return nil, fmt.Errorf("load catalog root %s: %w", own, err)
	}
	return cat, nil
}

// workload maps the case's optional workload to a providers.Workload (zero when unset).
func (c Case) workload() providers.Workload {
	if c.Workload == nil {
		return providers.Workload{}
	}
	return providers.Workload{Namespace: c.Workload.Namespace, Name: c.Workload.Name}
}

// recallConfig returns the case's recall gates with production defaults filled in for
// any zero field — mirroring config.load's InstantRecall defaults so a replay case
// recalls under the same thresholds production uses unless it explicitly tunes them.
func (c Case) recallConfig() CaseRecall {
	rc := CaseRecall{}
	if c.Recall != nil {
		rc = *c.Recall
	}
	if rc.MinScore == 0 {
		rc.MinScore = 1.0
	}
	if rc.MarginGap == 0 {
		rc.MarginGap = 1.0
	}
	if rc.SoloFloor == 0 {
		rc.SoloFloor = 4.0
	}
	if rc.OutcomePrior == 0 {
		rc.OutcomePrior = 2.0
	}
	if rc.OutcomeFloor == 0 {
		rc.OutcomeFloor = 0.5
	}
	return rc
}
