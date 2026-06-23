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
)

// Case is one replayable incident.
type Case struct {
	Name     string            `yaml:"name"`
	Prompt   string            `yaml:"prompt"` // the incident description (seeds the loop)
	Tools    map[string]string `yaml:"tools"`  // tool name -> recorded evidence the tool returns
	Expected Expected          `yaml:"expected"`
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
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
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
		cases = append(cases, c)
	}
	return cases, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
