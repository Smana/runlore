// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// forgeConfigured returns a Config with working GitHub App credentials (a real
// throwaway key in an env var), the minimum BuildForgeTokenSource accepts.
func forgeConfigured(t *testing.T) *config.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	t.Setenv("TEST_SWEEP_GH_KEY", pemStr)
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	cfg.Forge.GitHubApp = config.GitHubApp{AppID: 1, InstallationID: 2, PrivateKeyEnv: "TEST_SWEEP_GH_KEY"}
	return cfg
}

func TestBuildSweeperNilWhenOff(t *testing.T) {
	cfg := forgeConfigured(t)
	cfg.Curate.Sweeps.Mode = config.SweepOff
	if sw := BuildSweeper(cfg, nil, audit.Nop{}, discardLog()); sw != nil {
		t.Fatal("mode: off must not build a sweeper")
	}
}

func TestBuildSweeperNilWithoutForge(t *testing.T) {
	// No GitHub App / kb_repo: sweeps silently stay off — no new required config.
	if sw := BuildSweeper(&config.Config{}, nil, audit.Nop{}, discardLog()); sw != nil {
		t.Fatal("unconfigured forge must not build a sweeper")
	}
}

func TestBuildSweeperDefaultsToDryRunAgent(t *testing.T) {
	cfg := forgeConfigured(t) // Mode unset ⇒ dry-run
	sw := BuildSweeper(cfg, nil, audit.Nop{}, discardLog())
	if sw == nil {
		t.Fatal("configured forge + default mode must build a sweeper")
	}
	if len(sw.Agent.Passes) != 3 {
		t.Fatalf("nil ledger sweeper: want 3 forge-only passes, got %d", len(sw.Agent.Passes))
	}
}

// TestBuildSweeperWarnsWhenForgeUnusable pins the anti-silence contract. Reaching
// the nil return means the operator explicitly enabled sweeps, so returning nil
// without a word is indistinguishable from sweeps running and grooming nothing —
// the backlog rots and the logs never say why. A GitLab deployment hits this path
// on every start, since Phase-2 grooming is GitHub-only.
func TestBuildSweeperWarnsWhenForgeUnusable(t *testing.T) {
	// provider is written straight onto the config; "" exercises the empty-default
	// path, which must still report as "github" in the log.
	for _, tc := range []struct {
		name         string
		provider     string
		wantProvider string
	}{
		{"gitlab provider has no grooming code path", "gitlab", "gitlab"},
		{"github with no app credentials", "", "github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Forge.Provider = tc.provider
			cfg.Forge.KBRepo = "acme/kb"
			cfg.Curate.Sweeps.Mode = config.SweepApply

			var buf bytes.Buffer
			if sw := BuildSweeper(cfg, nil, audit.Nop{}, captureLog(&buf)); sw != nil {
				t.Fatal("an unusable forge must not build a sweeper")
			}
			rec := lastRecord(t, &buf)
			if rec["level"] != "WARN" {
				t.Fatalf("disabling sweeps must WARN, got level %v", rec["level"])
			}
			msg, _ := rec["msg"].(string)
			if !strings.Contains(msg, "curate sweeps disabled") {
				t.Fatalf("log message must name what was disabled, got %q", msg)
			}
			if rec["provider"] != tc.wantProvider {
				t.Fatalf("log must name the forge provider: got %v want %q", rec["provider"], tc.wantProvider)
			}
		})
	}
}
