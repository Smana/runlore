package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadScenarios(t *testing.T) {
	dir := t.TempDir()
	yaml := `
id: gitops-bad-image-tag
category: what-changed
description: bad image tag -> ImagePullBackOff
invasive: true
setup:
  - kubectl apply -f manifests/bad-tag.yaml
trigger:
  mode: cli
  symptom: app eval-victim pods not starting in ns runlore-eval
  namespace: runlore-eval
ground_truth:
  root_cause: image tag :v9.9.9 does not exist
  expected_sources: [gitops, kubernetes, logs]
  optional_sources: []
  expected_action: correct the image tag / flux rollback
  must_reach_root: true
teardown:
  - kubectl delete -f manifests/bad-tag.yaml --ignore-not-found
`
	if err := os.WriteFile(filepath.Join(dir, "s1.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	scns, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	if len(scns) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(scns))
	}
	s := scns[0]
	if s.ID != "gitops-bad-image-tag" || !s.Invasive || s.Trigger.Mode != "cli" {
		t.Fatalf("parse: %+v", s)
	}
	if len(s.GroundTruth.ExpectedSources) != 3 || !s.GroundTruth.MustReachRoot {
		t.Fatalf("ground_truth: %+v", s.GroundTruth)
	}
	if len(s.Setup) != 1 || len(s.Teardown) != 1 {
		t.Fatalf("steps: setup=%v teardown=%v", s.Setup, s.Teardown)
	}
}

func TestLoadScenariosIDFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "harbor.yaml"), []byte("category: dependency\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scns, err := LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scns) != 1 || scns[0].ID != "harbor" {
		t.Fatalf("want id=harbor from filename, got %+v", scns)
	}
}
