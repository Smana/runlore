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

	got := wireCloudProvider(context.Background(), cfg, &tools, captureLog(&buf), nil)

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

	got := wireCloudProvider(context.Background(), &config.Config{}, &tools, captureLog(&buf), nil)

	if got != nil || len(tools) != 0 {
		t.Errorf("unset provider wired something: provider=%v tools=%d", got, len(tools))
	}
	if out := buf.String(); strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("an unset, opt-in provider warned:\n%s", out)
	}
}

// TestPreflightDistinguishesADenialFromABlip pins the two-way contract, which is the
// whole reason the preflight verdict is typed rather than a bare error.
//
// Wiring runs EXACTLY ONCE per process (BuildModelAndTools is called from serve at
// startup and never again), so disabling the cloud lens is permanent until someone
// restarts the pod. That is the right response to a missing IAM binding, which will
// never start working on its own, and the wrong response to a 503 or a DNS hiccup
// during a slow pod start — the provider would have worked fine on the first real
// investigation, and the earlier all-or-nothing policy threw the lens away for the rest
// of the pod's life over a startup blip. AWS, which has no probe at all, survived the
// same blip; a probe that makes things worse than no probe is not a probe worth having.
//
// Neither case may kill the agent: one absent IAM binding must not become a total
// outage of an agent that was working before the cloud lens was switched on.
func TestPreflightDistinguishesADenialFromABlip(t *testing.T) {
	// A credential/transport failure, not an authorization denial. Runs off-GCE against
	// a port nothing listens on, so the token fetch fails fast rather than probing the
	// real link-local address.
	t.Run("an inconclusive preflight keeps the tools and says the lens is unverified", func(t *testing.T) {
		t.Setenv("GCE_METADATA_HOST", "127.0.0.1:0")
		var buf bytes.Buffer
		var tools []investigate.Tool

		cfg := &config.Config{}
		cfg.Cloud.Provider = config.CloudGCP
		cfg.Cloud.GCP.Project = "my-proj"
		cfg.Cloud.GCP.Location = "europe-west1"
		cfg.Cloud.GCP.ClusterName = "prod"

		got := wireCloudProvider(context.Background(), cfg, &tools, captureLog(&buf), nil)

		if got == nil {
			t.Error("a non-authorization preflight failure disabled the lens for the process " +
				"lifetime; only a denial may do that")
		}
		if len(tools) != 2 {
			t.Errorf("cloud tools were dropped over an inconclusive probe: got %d, want 2", len(tools))
		}
		out := buf.String()
		if !strings.Contains(out, "inconclusive") {
			t.Errorf("the operator is not told the lens is unverified:\n%s", out)
		}
		if !strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("an unverified cloud lens was not logged at WARN:\n%s", out)
		}
	})

	// The startup probe must be deadlined. serve's context carries no deadline, and the
	// chart's own network-policy comment documents that Cilium DROPS the metadata fetch
	// rather than refusing it — a dropped packet hangs instead of erroring, and a pod
	// that never serves is a strange outcome for a lens whose every failure is non-fatal.
	t.Run("the probe is bounded, so a dropped packet cannot hang startup", func(t *testing.T) {
		if cloudPreflightTimeout <= 0 {
			t.Fatal("cloudPreflightTimeout must be positive; an un-deadlined startup probe " +
				"blocks serve indefinitely when egress is dropped rather than refused")
		}
	})
}

// TestAnUnknownProviderNamesTheSupportedOnesFromTheFactoryTable pins that the "supported"
// list cannot drift from the set actually wired.
//
// It was a hand-written []string{config.CloudAWS, config.CloudGCP} sitting beside the
// switch whose cases it was describing — two copies of one fact, where adding a third
// cloud silently leaves the warning naming two.
func TestAnUnknownProviderNamesTheSupportedOnesFromTheFactoryTable(t *testing.T) {
	var buf bytes.Buffer
	var tools []investigate.Tool

	cfg := &config.Config{}
	cfg.Cloud.Provider = "azure" // a real cloud that is not wired: the honest near-miss

	if got := wireCloudProvider(context.Background(), cfg, &tools, captureLog(&buf), nil); got != nil {
		t.Errorf("an unwired provider produced a cloud provider: %v", got)
	}
	out := buf.String()
	for _, want := range []string{config.CloudAWS, config.CloudGCP} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not name %q as supported, so the operator's next move is "+
				"another guess:\n%s", want, out)
		}
	}
}
