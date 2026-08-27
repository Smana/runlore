// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

// TestAnUnknownCloudProviderIsNotSilent pins the fix for a genuine silent failure.
//
// The wiring this replaced was `if cfg.Cloud.Provider == "aws"` with no else. Setting
// any other value — a typo, or `gcp` in the window before it was implemented —
// registered no tools, wrote no log line and returned no error. The operator saw a
// clean startup and an agent that simply had no cloud lens, with nothing anywhere to
// explain it. Silence is the worst possible response to a key someone deliberately set.
func TestAnUnknownCloudProviderIsNotSilent(t *testing.T) {
	var buf bytes.Buffer
	var tools []investigate.Tool

	cfg := &config.Config{}
	cfg.Cloud.Provider = "gcpp" // the shape of a real typo, not a nonsense value

	got := wireCloudProvider(context.Background(), cfg, &tools, captureLog(&buf))

	if got != nil {
		t.Errorf("an unknown provider produced a cloud provider: %v", got)
	}
	if len(tools) != 0 {
		t.Errorf("an unknown provider registered %d tools", len(tools))
	}
	out := buf.String()
	for _, want := range []string{"unknown cloud.provider", "gcpp"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log does not contain %q, so the operator has no way to find this:\n%s", want, out)
		}
	}
	// Naming the value is not enough on its own — the reader also needs to know what
	// they could have written instead, or the next thing they try is another guess.
	for _, want := range []string{"aws", "gcp"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not name %q as a supported provider:\n%s", want, out)
		}
	}
}

// TestCloudContextOffStaysQuiet: cloud context is opt-in, so an unset provider must not
// warn. A warning every operator sees and cannot act on is how real warnings get
// filtered out.
func TestCloudContextOffStaysQuiet(t *testing.T) {
	var buf bytes.Buffer
	var tools []investigate.Tool

	got := wireCloudProvider(context.Background(), &config.Config{}, &tools, captureLog(&buf))

	if got != nil || len(tools) != 0 {
		t.Errorf("unset provider wired something: provider=%v tools=%d", got, len(tools))
	}
	if out := buf.String(); strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("an unset, opt-in provider warned:\n%s", out)
	}
}

// TestGCPPreflightFailureDisablesToolsWithoutKillingTheAgent pins the non-fatal
// contract, matching the AWS branch. A missing Workload Identity binding should cost
// the cloud lens, not the whole agent: refusing to start would turn one absent IAM
// binding into a total outage of an agent that was working before the lens was
// switched on.
//
// It runs off-GCE with no ADC, which is exactly the failure this must survive.
func TestGCPPreflightFailureDisablesToolsWithoutKillingTheAgent(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:0") // fail fast rather than probing the real link-local address
	var buf bytes.Buffer
	var tools []investigate.Tool

	cfg := &config.Config{}
	cfg.Cloud.Provider = config.CloudGCP
	cfg.Cloud.GCP.Project = "my-proj"
	cfg.Cloud.GCP.Location = "europe-west1"
	cfg.Cloud.GCP.ClusterName = "prod"

	got := wireCloudProvider(context.Background(), cfg, &tools, captureLog(&buf))

	if got != nil {
		t.Errorf("an unusable GCP provider was returned rather than disabled: %v", got)
	}
	if len(tools) != 0 {
		t.Errorf("cloud tools were registered despite the provider being unavailable: %d", len(tools))
	}
	if out := buf.String(); !strings.Contains(out, "cloud tools disabled") {
		t.Errorf("the operator is not told the cloud lens is off:\n%s", out)
	}
}
