// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRecordedCase(t *testing.T) {
	scn := Scenario{ID: "harbor-valkey", Trigger: Trigger{Symptom: "harbor core crashlooping"},
		GroundTruth: GroundTruth{RootCause: "valkey down", ExpectedSources: []string{"kubernetes"}}}
	calls := []Call{
		{Name: "pod_status", Output: "first"},
		{Name: "pod_status", Output: "second"}, // last wins (v1 single-output replay limit)
		{Name: "query_logs", Output: "connection refused"},
		{Name: "submit_findings", Output: "ignored"}, // reserved, excluded
	}
	c := RecordedCase(scn, calls)
	if c.Name != "harbor-valkey" || c.Prompt != "harbor core crashlooping" {
		t.Fatalf("meta: %+v", c)
	}
	if c.Tools["pod_status"] != "second" || c.Tools["query_logs"] != "connection refused" {
		t.Fatalf("tools: %+v", c.Tools)
	}
	if _, ok := c.Tools["submit_findings"]; ok {
		t.Fatal("submit_findings must be excluded")
	}
	if c.GroundTruth == nil || c.GroundTruth.RootCause != "valkey down" ||
		len(c.GroundTruth.ExpectedSources) != 1 || c.GroundTruth.ExpectedSources[0] != "kubernetes" {
		t.Fatalf("recorded case must carry the scenario ground truth: %+v", c.GroundTruth)
	}
}

func TestWriteCase(t *testing.T) {
	dir := t.TempDir()
	c := Case{Name: "x", Prompt: "p", Tools: map[string]string{"pod_status": "Pending"}}
	if err := WriteCase(dir, c); err != nil {
		t.Fatalf("WriteCase: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "x.yaml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got Case
	if err := yaml.Unmarshal(data, &got); err != nil || got.Name != "x" || got.Tools["pod_status"] != "Pending" {
		t.Fatalf("roundtrip: %+v err=%v", got, err)
	}
}
