// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    providers.Workload
		wantErr bool
	}{
		{
			name: "namespace and name",
			in:   "apps/checkout-api",
			want: providers.Workload{Namespace: "apps", Name: "checkout-api"},
		},
		{
			name: "kind namespace and name",
			in:   "Deployment/apps/checkout-api",
			want: providers.Workload{Kind: "Deployment", Namespace: "apps", Name: "checkout-api"},
		},
		{name: "empty", in: "", wantErr: true},
		{name: "single segment", in: "checkout-api", wantErr: true},
		{name: "trailing slash", in: "apps/", wantErr: true},
		{name: "leading slash", in: "/checkout-api", wantErr: true},
		{name: "empty middle segment", in: "Deployment//checkout-api", wantErr: true},
		{name: "four segments", in: "a/b/c/d", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTarget(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTarget(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseTarget(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseInvestigateArgs(t *testing.T) {
	t.Run("interleaved target and flags", func(t *testing.T) {
		opts, err := parseInvestigateArgs([]string{
			"apps/checkout-api", "--provider", "gemini", "--effort", "high",
			"--kube-context", "prod", "--json", "--quiet",
		})
		if err != nil {
			t.Fatalf("parseInvestigateArgs: %v", err)
		}
		wantW := providers.Workload{Namespace: "apps", Name: "checkout-api"}
		if opts.workload != wantW {
			t.Errorf("workload = %+v, want %+v", opts.workload, wantW)
		}
		if opts.title != "Investigate apps/checkout-api" {
			t.Errorf("title = %q, want %q", opts.title, "Investigate apps/checkout-api")
		}
		if opts.provider != "gemini" || opts.effort != "high" || opts.kubeContext != "prod" || !opts.jsonOut || !opts.quiet {
			t.Errorf("unexpected opts: %+v", opts)
		}
	})

	t.Run("conflicting namespace and target fails", func(t *testing.T) {
		_, err := parseInvestigateArgs([]string{"apps/checkout-api", "--namespace", "other"})
		if err == nil {
			t.Fatal("expected conflict error when --namespace differs from target namespace")
		}
	})

	t.Run("matching namespace and target succeeds", func(t *testing.T) {
		opts, err := parseInvestigateArgs([]string{"apps/checkout-api", "--namespace", "apps"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.workload.Namespace != "apps" || opts.workload.Name != "checkout-api" {
			t.Errorf("workload = %+v", opts.workload)
		}
	})

	t.Run("two positional args rejected", func(t *testing.T) {
		_, err := parseInvestigateArgs([]string{"apps/a", "apps/b"})
		if err == nil {
			t.Fatal("expected error for two positional args")
		}
	})

	t.Run("no target alert or message rejected", func(t *testing.T) {
		_, err := parseInvestigateArgs([]string{"--namespace", "apps"})
		if err == nil {
			t.Fatal("expected error when none of target, --alert, or --message is set")
		}
	})
}

func TestApplyInvestigateOverridesValidatesAfterOverrides(t *testing.T) {
	t.Run("invalid effort fails fast", func(t *testing.T) {
		cfg := &config.Config{Model: config.Model{
			Provider:  "gemini",
			Model:     "gemini-4-neon",
			APIKeyEnv: "GEMINI_API_KEY",
		}}
		config.ApplyDefaults(cfg)
		err := applyInvestigateOverrides(cfg, investigateOptions{effort: "ludicrous"})
		if err == nil {
			t.Fatal("expected validation error for invalid --effort")
		}
	})

	t.Run("cleartext public base-url with key fails fast", func(t *testing.T) {
		cfg := &config.Config{Model: config.Model{
			Provider:  "openai",
			BaseURL:   "https://api.openai.com/v1",
			Model:     "gpt-4o-mini",
			APIKeyEnv: "OPENAI_API_KEY",
		}}
		config.ApplyDefaults(cfg)
		err := applyInvestigateOverrides(cfg, investigateOptions{baseURL: "http://gpu.public.example/v1"})
		if err == nil {
			t.Fatal("expected validation error for cleartext public --base-url with API key")
		}
	})
}

func TestFormatToolCallProgressRedactsSecrets(t *testing.T) {
	line := formatToolCallProgress(providers.ToolCall{
		Name: "query_logs",
		Args: `{"namespace":"apps","token":"ghp_1234567890abcdef1234567890abcdef1234"}`,
	})
	if !strings.HasPrefix(line, "→ query_logs(") {
		t.Errorf("line = %q, want prefix '→ query_logs('", line)
	}
	if strings.Contains(line, "ghp_1234567890abcdef1234567890abcdef1234") {
		t.Errorf("secret leaked in progress line: %q", line)
	}
}

func TestWriteInvestigationJSON(t *testing.T) {
	var buf bytes.Buffer
	inv := providers.Investigation{
		Title:      "Investigate apps/checkout-api",
		Confidence: 0.9,
		Verdict:    providers.VerdictActionSuggested,
	}
	if err := writeInvestigationJSON(&buf, inv); err != nil {
		t.Fatalf("writeInvestigationJSON: %v", err)
	}
	var decoded providers.Investigation
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if decoded.Title != inv.Title || decoded.Confidence != inv.Confidence {
		t.Errorf("decoded = %+v, want %+v", decoded, inv)
	}
}
