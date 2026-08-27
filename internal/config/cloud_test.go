// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCloudGCPBlockIsFullyOptional pins the zero-configuration promise: on GKE every
// GCP field is autodetected, so `cloud: {provider: gcp}` alone must parse and leave
// the block empty for the provider to fill in. A required field here would break the
// project's standing constraint that no integration adds mandatory config.
func TestCloudGCPBlockIsFullyOptional(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("cloud:\n  provider: gcp\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.Provider != CloudGCP {
		t.Errorf("provider = %q, want %q", c.Cloud.Provider, CloudGCP)
	}
	if c.Cloud.GCP.Project != "" || c.Cloud.GCP.Location != "" || c.Cloud.GCP.ClusterName != "" {
		t.Errorf("expected an empty GCP block, got %+v", c.Cloud.GCP)
	}
}

// TestCloudGCPBlockRoundTrips covers the escape hatch — the off-GKE or cross-project
// operator who must state all three explicitly.
func TestCloudGCPBlockRoundTrips(t *testing.T) {
	in := "cloud:\n  provider: gcp\n  gcp:\n    project: my-proj\n    location: europe-west1\n    cluster_name: prod\n"
	var c Config
	if err := yaml.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.GCP.Project != "my-proj" {
		t.Errorf("project = %q, want my-proj", c.Cloud.GCP.Project)
	}
	if c.Cloud.GCP.Location != "europe-west1" {
		t.Errorf("location = %q, want europe-west1", c.Cloud.GCP.Location)
	}
	if c.Cloud.GCP.ClusterName != "prod" {
		t.Errorf("cluster_name = %q, want prod", c.Cloud.GCP.ClusterName)
	}
}

// TestCloudAWSFlatFieldsStillParse is the non-regression half: adding a nested block
// must not disturb the flat AWS spelling that every existing deployment uses.
func TestCloudAWSFlatFieldsStillParse(t *testing.T) {
	in := "cloud:\n  provider: aws\n  region: eu-west-3\n  cluster_name: prod\n"
	var c Config
	if err := yaml.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.Provider != CloudAWS || c.Cloud.Region != "eu-west-3" || c.Cloud.ClusterName != "prod" {
		t.Errorf("flat AWS fields did not round-trip: %+v", c.Cloud)
	}
}
