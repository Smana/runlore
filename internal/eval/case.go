// SPDX-License-Identifier: Apache-2.0

// Package eval replays recorded incident cases through the investigation loop and
// scores whether the agent identifies the root cause — a reproducible RCA benchmark
// (cf. ITBench). A case records the evidence each tool returns, so the eval measures
// the model+loop's reasoning over fixed evidence, independent of a live cluster.
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
)

// Case is one replayable incident.
type Case struct {
	Name     string            `yaml:"name"`
	Prompt   string            `yaml:"prompt"` // the incident description (seeds the loop)
	Tools    map[string]string `yaml:"tools"`  // tool name -> recorded evidence the tool returns
	Expected Expected          `yaml:"expected"`
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
	// Recall optionally tunes the recall gates for this case (mirrors config
	// instant_recall). Absent (or a zero field) ⇒ the production default. Consulted only
	// when CatalogDir is set.
	Recall *CaseRecall `yaml:"recall,omitempty"`
	// ExpectRecall asserts the recall outcome mechanically and fails the case when unmet:
	//   short_circuit — recall fired and its answer was delivered (loop skipped)
	//   withdrawn     — recall fired but the verify pass rejected it and the loop fell
	//                   through to a full investigation
	//   fired         — recall fired (either short_circuit or withdrawn)
	//   rejected      — a recall gate rejected the hit: recall never fired
	// Empty ⇒ no recall assertion (existing cases are unaffected).
	ExpectRecall string `yaml:"expect_recall,omitempty"`

	// dir is the directory the case file was loaded from, used to resolve CatalogDir.
	// Set by Load; unexported so YAML never populates it.
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
type Expected struct {
	MustContain       []string `yaml:"must_contain"`        // keywords that must appear in the findings (recall, over full findings text)
	MinConfidence     float64  `yaml:"min_confidence"`      // confidence floor (0 = no floor)
	RootCauseEntities []string `yaml:"root_cause_entities"` // entities that MUST be named as the cause (entity recall, over claim text)
	Distractors       []string `yaml:"distractors"`         // plausible-but-wrong entities that must NOT be blamed (over-claim/FP); only evaluated when root_cause_entities is non-empty
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
		c.dir = dir // resolve CatalogDir relative to the case file's directory
		cases = append(cases, c)
	}
	return cases, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
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
