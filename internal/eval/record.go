package eval

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RecordedCase converts a live run's recorded tool calls into a replayable Case
// (the existing examples/eval format). v1 keeps the LAST output per tool: the
// replay staticTool returns one fixed output per tool regardless of args, so
// multi-call tools are flattened. submit_findings is excluded (it is the model's
// own output, not evidence).
func RecordedCase(scn Scenario, calls []Call) Case {
	tools := map[string]string{}
	for _, c := range calls {
		if c.Name == "submit_findings" {
			continue
		}
		tools[c.Name] = c.Output
	}
	return Case{
		Name:   scn.ID,
		Prompt: scn.Trigger.Symptom,
		Tools:  tools,
		Expected: Expected{
			MustContain:   nil, // authored later when promoting a fixture into the regression set
			MinConfidence: 0,
		},
	}
}

// WriteCase writes a Case as <dir>/<name>.yaml.
func WriteCase(dir string, c Case) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, c.Name+".yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write case %s: %w", path, err)
	}
	return nil
}
