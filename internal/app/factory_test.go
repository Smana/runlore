// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/factory"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
)

func factoryCfg(dir string) *config.Config {
	return &config.Config{
		Outcome: config.Outcome{LedgerPath: filepath.Join(dir, "outcomes.jsonl")},
		Factory: config.Factory{
			URL: "http://factory:8082", TokenURL: "https://auth.example.com/oauth/v2/token",
			ClientIDEnv: "FT_ID", ClientSecretEnv: "FT_SECRET", ProjectIDEnv: "FT_PROJ", // pragma: allowlist secret
			Repository: "Smana/cloud-native-ref", MaxCostUSD: 2, Webhook: config.FactoryWebhook{SecretEnv: "FT_HOOK"},
		},
	}
}

func TestBuildFactory(t *testing.T) {
	t.Setenv("FT_ID", "runlore")
	t.Setenv("FT_SECRET", "s")
	t.Setenv("FT_PROJ", "p")
	t.Setenv("FT_HOOK", "k")
	dir := t.TempDir()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	l, c, err := BuildFactory(factoryCfg(dir), log)
	if err != nil || l == nil || c == nil || !c.Enabled() {
		t.Fatalf("on: %v %v %v", l, c, err)
	}
	if err := l.Upsert("k", func(*factory.Record) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "factory.jsonl")); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "factory.jsonl")); len(m) != 1 {
		t.Fatal("the ledger is factory.jsonl beside the outcome ledger")
	}
	if logs.Len() != 0 {
		t.Fatalf("no warning when every env is set: %s", logs.String())
	}

	off := factoryCfg(dir)
	off.Factory.URL = ""
	if l, c, err := BuildFactory(off, log); l != nil || c != nil || err != nil {
		t.Fatalf("url empty: both nil, got %v %v %v", l, c, err)
	}

	t.Setenv("FT_SECRET", "")
	logs.Reset()
	l, c, err = BuildFactory(factoryCfg(dir), log)
	if err != nil || l == nil || c == nil || c.Enabled() {
		t.Fatalf("an empty env disables the client: %v %v %v", l, c, err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "FT_SECRET") {
		t.Fatalf("exactly one warning naming the env var: %s", logs.String())
	}
	if strings.Contains(logs.String(), "runlore") {
		t.Fatalf("a credential value was logged: %s", logs.String())
	}
}

func TestBuildFactoryReceiverReadsTheKeyAtEachRequest(t *testing.T) {
	t.Setenv("FT_HOOK", "")
	l, _, _ := BuildFactory(factoryCfg(t.TempDir()), slog.New(slog.DiscardHandler))
	rc := BuildFactoryReceiver(factoryCfg(t.TempDir()), l, nil, slog.New(slog.DiscardHandler))
	if rc == nil {
		t.Fatal("a receiver when the factory is on")
	}
	if rc.Keys() != nil {
		t.Fatal("no key in the env: no keys, so every request is refused")
	}
	t.Setenv("FT_HOOK", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=") // 32 bytes
	if len(rc.Keys()) != 1 {
		t.Fatal("the key is read from the environment at each request")
	}
	off := factoryCfg(t.TempDir())
	off.Factory.URL = ""
	if BuildFactoryReceiver(off, nil, nil, slog.New(slog.DiscardHandler)) != nil {
		t.Fatal("off: no receiver")
	}
}

func TestBuildFactoryVerifier(t *testing.T) {
	t.Setenv("FT_ID", "runlore")
	t.Setenv("FT_SECRET", "s")
	t.Setenv("FT_PROJ", "p")
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	cfg := factoryCfg(t.TempDir())
	cfg.Factory.Soak, cfg.Factory.Verify.MaxSteps = config.Duration(30*time.Minute), 6
	ledger, client, err := BuildFactory(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	// No alertmanager_url: verification is off.
	if v := BuildFactoryVerifier(cfg, nil, ledger, client, nil, nil, log); v != nil {
		t.Fatal("verification needs factory.verify.alertmanager_url")
	}
	cfg.Factory.Verify.AlertmanagerURL = "http://alertmanager:9093"
	v := BuildFactoryVerifier(cfg, nil, ledger, client, nil, nil, log)
	if v == nil || v.Soak != 30*time.Minute || v.Firing == nil || v.Ledger != ledger {
		t.Fatalf("%+v", v)
	}
	// No model (nil deps): the verdict is still taken, with fixed evidence.
	if v.Reinvestigate != nil {
		t.Fatal("no model, no re-investigation")
	}
	// A disabled client builds nothing.
	t.Setenv("FT_SECRET", "")
	if _, c2, _ := BuildFactory(cfg, log); BuildFactoryVerifier(cfg, nil, ledger, c2, nil, nil, log) != nil {
		t.Fatal("a disabled client verifies nothing")
	}
}

func TestVerifyRequest(t *testing.T) {
	req := verifyRequest(factory.Record{TriggerKey: "k1", Alert: "KubePodCrashLooping", Namespace: "apps", Workload: "Deployment/web", PR: 77})
	if req.TriggerKey != "k1" || req.Title != "Verify the fix for KubePodCrashLooping" || req.Source != investigate.SourceAlert ||
		req.Workload.Kind != "Deployment" || req.Workload.Name != "web" || req.Workload.Namespace != "apps" ||
		req.Reason != "the fix merged in #77 should have resolved this alert; check whether it still fires and why" {
		t.Fatalf("%+v", req)
	}
	if r := verifyRequest(factory.Record{TriggerKey: "k2", Alert: "A"}); r.Workload.Name != "" {
		t.Fatalf("no workload in the record: %+v", r.Workload)
	}
}

// curationCfg is factoryCfg with working forge credentials and factory.curation.defer on.
func curationCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := forgeConfigured(t)
	cfg.Outcome.LedgerPath = filepath.Join(t.TempDir(), "outcomes.jsonl")
	cfg.Factory = factoryCfg(t.TempDir()).Factory
	cfg.Factory.Curation = config.FactoryCuration{Defer: true, Window: config.Duration(48 * time.Hour)}
	return cfg
}

func curationRig(t *testing.T, cfg *config.Config, log *slog.Logger) (*factory.Curation, *notify.Multi) {
	t.Helper()
	ledger, err := factory.OpenLedger(filepath.Join(t.TempDir(), "factory.jsonl"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	client := factory.NewClient(cfg.Factory, func(string) string { return "x" }, nil, time.Now)
	outcomes, err := outcome.New(cfg.Outcome.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	cu := BuildFactoryCuration(cfg, ledger, client, outcomes, nil, audit.Nop{}, log)
	multi := notify.NewMulti(log)
	addFactoryNotifier(multi, cfg, ledger, client, cu, log)
	return cu, multi
}

func TestBuildFactoryCuration(t *testing.T) {
	t.Run("on: wired, and the Multi holds no factory notifier", func(t *testing.T) {
		cfg := curationCfg(t)
		cu, multi := curationRig(t, cfg, discardLog())
		if cu == nil {
			t.Fatal("defer on with a usable GitHub client: want a Curation")
		}
		if cu.Window != 48*time.Hour || cu.Repository != cfg.Factory.Repository || cu.Filer == nil || cu.Ledger == nil ||
			cu.Outcome == nil || cu.Pulls == nil || cu.Drafts == nil || cu.Resolve == nil || cu.FindEntry == nil {
			t.Errorf("unwired: %+v", cu)
		}
		if cu.Promote != nil {
			t.Error("Promote stays nil until the Queue's PromoteOne exists")
		}
		if !cu.DryRun {
			t.Error("curate.sweeps.mode unset is dry-run, and Curation.DryRun follows it")
		}
		if multi.Len() != 0 {
			t.Errorf("the Curation files, so the Multi must hold no factory notifier: %d", multi.Len())
		}
	})
	t.Run("apply mode is not a dry run", func(t *testing.T) {
		cfg := curationCfg(t)
		cfg.Curate.Sweeps.Mode = config.SweepApply
		if cu, _ := curationRig(t, cfg, discardLog()); cu == nil || cu.DryRun {
			t.Fatalf("apply: DryRun must be false: %+v", cu)
		}
	})
	t.Run("off: nothing changes", func(t *testing.T) {
		cfg := curationCfg(t)
		cfg.Factory.Curation.Defer = false
		cu, multi := curationRig(t, cfg, discardLog())
		if cu != nil {
			t.Fatal("defer off: want nil")
		}
		if multi.Len() != 1 {
			t.Errorf("defer off keeps today's factory notifier in the Multi: %d", multi.Len())
		}
	})
	t.Run("no GitHub client: nil, with a warning", func(t *testing.T) {
		cfg := curationCfg(t)
		cfg.Forge.GitHubApp = config.GitHubApp{}
		var logs bytes.Buffer
		cu, _ := curationRig(t, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
		if cu != nil {
			t.Fatal("no usable GitHub client: want nil")
		}
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "factory.curation") {
			t.Errorf("want a warning naming factory.curation: %s", logs.String())
		}
	})
	t.Run("FindEntry reports no hit without a catalog", func(t *testing.T) {
		cu, _ := curationRig(t, curationCfg(t), discardLog())
		if path, ok := cu.FindEntry("fp"); ok || path != "" {
			t.Errorf("nil catalog: want no hit, got %q %v", path, ok)
		}
	})
}

func TestBuildGitHubClient(t *testing.T) {
	cfg := forgeConfigured(t)
	if buildGitHubClient(cfg, discardLog()) == nil {
		t.Error("credentials and kb_repo: want a client")
	}
	for name, mutate := range map[string]func(*config.Config){
		"no kb_repo":     func(c *config.Config) { c.Forge.KBRepo = "" },
		"gitlab":         func(c *config.Config) { c.Forge.Provider = "gitlab" },
		"no credentials": func(c *config.Config) { c.Forge.GitHubApp = config.GitHubApp{} },
		"bad kb_repo":    func(c *config.Config) { c.Forge.KBRepo = "nope" },
	} {
		c := forgeConfigured(t)
		mutate(c)
		if buildGitHubClient(c, discardLog()) != nil {
			t.Errorf("%s: want nil", name)
		}
	}
}

// TestWithFactoryCurationSetsInner: BuildInvestigator hands the curator it built to the Curation,
// and a disabled curator leaves Inner a nil interface rather than a typed nil.
func TestWithFactoryCurationSetsInner(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	for name, withForge := range map[string]bool{"curator built": true, "no forge": false} {
		t.Run(name, func(t *testing.T) {
			cfg := forgeConfigured(t)
			if !withForge {
				cfg.Forge.GitHubApp = config.GitHubApp{}
			}
			cfg.Model = config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}
			deps := BuildDeps(context.Background(), cfg, nil, nil, nil, discardLog())
			cu := &factory.Curation{}
			if _, _, _, err := BuildInvestigator(context.Background(), cfg, deps, nil, nil, nil, nil, nil, discardLog(),
				WithFactoryCuration(cu)); err != nil {
				t.Fatal(err)
			}
			if (cu.Inner != nil) != withForge {
				t.Errorf("Inner set = %v, want %v", cu.Inner != nil, withForge)
			}
		})
	}
}
