// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/factory"
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
