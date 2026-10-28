// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/factory"
	"github.com/Smana/runlore/internal/investigate"
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
