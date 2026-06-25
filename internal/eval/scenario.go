package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one live-fire eval case: an induced-or-natural failure, how to
// trigger an investigation, and the ground truth to grade against. It is a
// superset of Case (which is the recorded/replay form this produces).
type Scenario struct {
	ID          string      `yaml:"id"`
	Category    string      `yaml:"category"` // what-changed | saturation | network | cloud | dependency | cert | dns | storage | instant-recall
	Description string      `yaml:"description"`
	Invasive    bool        `yaml:"invasive"` // true => has setup/teardown; false => natural failure
	Precheck    string      `yaml:"precheck"` // optional shell; non-zero exit => SKIP (natural scenarios)
	Setup       []string    `yaml:"setup"`    // shell steps (kubectl/flux) to induce the fault
	Trigger     Trigger     `yaml:"trigger"`
	GroundTruth GroundTruth `yaml:"ground_truth"`
	Teardown    []string    `yaml:"teardown"` // shell steps to revert; always run
}

// Trigger describes how the investigation is started.
type Trigger struct {
	Mode      string `yaml:"mode"`      // "cli" (default) | "webhook"
	Symptom   string `yaml:"symptom"`   // free-text incident description
	Namespace string `yaml:"namespace"` // affected namespace (optional)
}

// GroundTruth is the human-authored truth a scenario is graded against.
type GroundTruth struct {
	RootCause       string   `yaml:"root_cause"`
	ExpectedSources []string `yaml:"expected_sources"` // MANDATORY data-source groups -> coverage gate
	OptionalSources []string `yaml:"optional_sources"` // bonus if touched, never gates
	ExpectedAction  string   `yaml:"expected_action"`
	MustReachRoot   bool     `yaml:"must_reach_root"`
}

// LoadScenarios reads every *.yaml / *.yml scenario in dir.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("LoadScenarios %s: %w", dir, err)
	}
	var scns []Scenario
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: name comes from reading the operator-supplied scenarios dir
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := yaml.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		if s.Trigger.Mode == "" {
			s.Trigger.Mode = "cli"
		}
		scns = append(scns, s)
	}
	return scns, nil
}
