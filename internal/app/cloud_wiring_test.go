// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	gcpcloud "github.com/Smana/runlore/internal/providers/cloud/gcp"
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

// stubCloud is a providers.CloudProvider whose only interesting behaviour is the verdict
// its Preflight returns.
//
// A stub at the CloudProvider boundary, rather than a real GCP client, because
// enableCloudProvider is provider-agnostic: it reads nothing from config and knows about
// no cloud. Building a real provider to reach it made this an integration test of
// Application Default Credentials by accident — see the comment on the test below.
//
// The lenses return nothing rather than panicking: registration must not call them, and a
// nil answer says so without the failure mode being a stack trace in an unrelated test.
type stubCloud struct{ preflight error }

func (stubCloud) CloudChanges(context.Context, providers.Selector, providers.TimeWindow, providers.CloudChangeFilter) ([]providers.Change, error) {
	return nil, nil
}

func (stubCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}

func (s stubCloud) Preflight(context.Context) error { return s.preflight }

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
//
// This drives enableCloudProvider with a stub rather than wireCloudProvider with a real
// GCP client, and that is a correctness fix, not a convenience. The earlier version set
// GCE_METADATA_HOST to a dead port intending "no credentials, so the probe fails at the
// transport layer". On a machine with `gcloud` ADC configured — every developer laptop
// that has ever touched GCP — ADC ignores the dead metadata host, the client builds on
// the developer's own credentials, and the probe reaches the real Cloud Logging API and
// earns a genuine 403 for a project they cannot read. That is correctly a DENIAL, so the
// test failed locally and passed in CI, which is the worst way for a test to be wrong:
// it read as a regression in the code under test. Whether a transport failure classifies
// as inconclusive is now pinned where it belongs, against a fake Cloud Logging, in
// gcp.TestPreflightReportsATransportFailureAsInconclusive.
func TestPreflightDistinguishesADenialFromABlip(t *testing.T) {
	t.Run("an inconclusive preflight keeps the tools and says the lens is unverified", func(t *testing.T) {
		var buf bytes.Buffer
		var tools []investigate.Tool

		// Not wrapping ErrCloudPreflightDenied — the shape of a 503, a dropped packet or
		// a deadline during a slow pod start.
		cl := stubCloud{preflight: errors.New("cloud logging read failed on project my-proj: dial tcp: i/o timeout")}

		got := enableCloudProvider(context.Background(), config.CloudGCP, cl, &tools, captureLog(&buf))

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

	// The other direction. A denial is the one failure that genuinely cannot work until
	// someone changes a binding, so it is the only one allowed to be permanent — and the
	// pastable command is the entire reason the probe was worth adding.
	t.Run("a denial drops the tools and prints a pastable binding", func(t *testing.T) {
		var buf bytes.Buffer
		var tools []investigate.Tool

		cl := stubCloud{preflight: &gcpcloud.DeniedError{
			Summary: "cloud logging read denied on project my-proj",
			Command: "gcloud projects add-iam-policy-binding my-proj --role=roles/logging.viewer",
		}}

		stderr, restore := captureStderr(t)
		got := enableCloudProvider(context.Background(), config.CloudGCP, cl, &tools, captureLog(&buf))
		restore()

		if got != nil {
			t.Error("a denied preflight left the cloud lens enabled; every investigation that " +
				"reaches for it now 403s mid-run instead")
		}
		if len(tools) != 0 {
			t.Errorf("a denied preflight registered %d cloud tools", len(tools))
		}
		if out := buf.String(); !strings.Contains(out, "cloud tools disabled") {
			t.Errorf("the log does not say the lens was disabled:\n%s", out)
		}
		// On stderr, NOT in the structured log: under the chart's default JSON logging a
		// multi-line value arrives as one escaped string, and being pastable is the only
		// reason to generate a command at all.
		if e := stderr(); !strings.Contains(e, "add-iam-policy-binding") {
			t.Errorf("the remediation command did not reach stderr, so the operator has "+
				"nothing to paste:\n%s", e)
		}
		if strings.Contains(buf.String(), "add-iam-policy-binding") {
			t.Error("the command was written to the structured log, where JSON escaping makes " +
				"it unpastable")
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

// captureStderr redirects os.Stderr to a pipe and returns a reader for what was written
// plus the function that puts it back. Both are needed because the drain must happen
// before the assertion and the restore before the next subtest.
func captureStderr(t *testing.T) (func() string, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()

	var out string
	var once sync.Once
	restore := func() {
		once.Do(func() {
			os.Stderr = orig
			_ = w.Close()
			out = <-done
			_ = r.Close()
		})
	}
	t.Cleanup(restore)
	return func() string { restore(); return out }, restore
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

// TestCloudProviderWithoutAModelIsNotSilent pins the fix for the silence a first live
// GKE install hit (#562).
//
// Every investigation tool is wired inside BuildModelAndTools, and BuildDeps returns nil
// before reaching it when no model is configured. So `cloud: {provider: gcp}` on an
// install that had not yet configured a model produced NO GCP log line at all — not the
// resolved-identity line, not a preflight verdict, not even "cloud tools disabled". The
// operator's reasonable conclusion was that the cloud block had been ignored.
//
// The asymmetry is what makes it a defect rather than a missing nicety: every other
// outcome for that block says something. A typo warns, an unreachable metadata server
// warns, a denied binding warns and prints a command. Only the case that is nobody's
// fault is silent.
func TestCloudProviderWithoutAModelIsNotSilent(t *testing.T) {
	var buf bytes.Buffer

	cfg := &config.Config{}
	cfg.Cloud.Provider = config.CloudGCP // deliberately set, and inert without a model

	if deps := BuildDeps(context.Background(), cfg, nil, nil, nil, captureLog(&buf)); deps != nil {
		t.Fatal("BuildDeps returned deps with no model configured")
	}

	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("a deliberately-set cloud.provider was silently inert:\n%s", out)
	}
	// Naming the key is what separates this from a generic "no model" line: the operator
	// is looking for the fate of the block they wrote, not for a model warning.
	if !strings.Contains(out, "cloud.provider="+config.CloudGCP) {
		t.Errorf("the warning does not name the key that was set, so it does not answer the "+
			"question the operator actually has:\n%s", out)
	}
	// And the fix has to be in the line, or the operator has a symptom and no next move.
	if !strings.Contains(out, "model.provider") {
		t.Errorf("the warning does not name what to set to enable the tools:\n%s", out)
	}
}

// TestNoDatasourceConfiguredStaysQuietWithoutAModel: running without a model is a
// supported mode — the GitOps watch alone is useful, and the issue above reports it
// flagging four real failures before any model existed. It must not warn about a
// datasource nobody configured.
func TestNoDatasourceConfiguredStaysQuietWithoutAModel(t *testing.T) {
	var buf bytes.Buffer

	if deps := BuildDeps(context.Background(), &config.Config{}, nil, nil, nil, captureLog(&buf)); deps != nil {
		t.Fatal("BuildDeps returned deps with no model configured")
	}
	if out := buf.String(); strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("a model-less install with no cloud block warned about one:\n%s", out)
	}
}
