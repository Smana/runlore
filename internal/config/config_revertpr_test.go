// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestValidateRevertPR(t *testing.T) {
	baseApprove := func() Config {
		return Config{
			Actions: ActionPolicy{
				Mode:             ActionApprove,
				ApprovalTokenEnv: "RUNLORE_APPROVAL_TOKEN",
				AuditLogPath:     "/var/lib/runlore/audit.jsonl",
				RevertPR: RevertPRPolicy{
					Enabled:      true,
					AllowedRepos: []string{"org/gitops-infra"},
					Forge: &RevertPRForge{
						Provider: "github",
						GitHubApp: GitHubApp{
							AppID:          123,
							InstallationID: 456,
							PrivateKeyEnv:  "REVERT_GH_KEY",
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "valid dedicated github forge",
			mutate: func(*Config) {},
		},
		{
			name: "valid dedicated gitlab forge",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = &RevertPRForge{
					Provider: "gitlab",
					GitLab: GitLab{
						BaseURL:  "https://gitlab.example.com",
						TokenEnv: "REVERT_GL_TOKEN",
					},
				}
			},
		},
		{
			name: "valid reuse_forge_credentials with top-level github app",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = nil
				c.Actions.RevertPR.ReuseForgeCredentials = true
				c.Forge = Forge{
					Provider: "github",
					GitHubApp: GitHubApp{
						AppID:          1,
						InstallationID: 2,
						PrivateKeyEnv:  "FORGE_GH_KEY",
					},
				}
			},
		},
		{
			name: "valid reuse_forge_credentials with top-level gitlab",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = nil
				c.Actions.RevertPR.ReuseForgeCredentials = true
				c.Forge = Forge{
					Provider: "gitlab",
					GitLab:   GitLab{TokenEnv: "FORGE_GL_TOKEN"},
				}
			},
		},
		{
			name: "disabled revert_pr passes even under off mode with empty fields",
			mutate: func(c *Config) {
				c.Actions.Mode = ActionOff
				c.Actions.RevertPR = RevertPRPolicy{Enabled: false}
			},
		},
		{
			name: "enabled under auto mode rejected",
			mutate: func(c *Config) {
				c.Actions.Mode = ActionAuto
				c.Server.WebhookTokenEnv = "WH_TOK"
				c.Actions.Auto.MinConfidence = 0.8
				c.Actions.Auto.MaxPerWindow = 5
				c.Actions.Allow.Namespaces = []string{"apps"}
			},
			wantErr: "approve",
		},
		{
			name: "enabled under off mode rejected",
			mutate: func(c *Config) {
				c.Actions.Mode = ActionOff
			},
			wantErr: "approve",
		},
		{
			name: "enabled under suggest mode rejected",
			mutate: func(c *Config) {
				c.Actions.Mode = ActionSuggest
			},
			wantErr: "approve",
		},
		{
			name: "empty allowed_repos rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.AllowedRepos = nil
			},
			wantErr: "allowed_repos",
		},
		{
			name: "malformed allowed_repos entry rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.AllowedRepos = []string{"just-a-name"}
			},
			wantErr: "allowed_repos",
		},
		{
			name: "missing forge and reuse_forge_credentials false rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = nil
				c.Actions.RevertPR.ReuseForgeCredentials = false
			},
			wantErr: "reuse_forge_credentials",
		},
		{
			name: "reuse_forge_credentials true but top-level forge has no credentials rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = nil
				c.Actions.RevertPR.ReuseForgeCredentials = true
				c.Forge = Forge{Provider: "github"}
			},
			wantErr: "forge",
		},
		{
			name: "dedicated forge unknown provider rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge.Provider = "bitbucket"
			},
			wantErr: "provider",
		},
		{
			name: "dedicated gitlab forge missing token_env rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = &RevertPRForge{
					Provider: "gitlab",
					GitLab:   GitLab{BaseURL: "https://gitlab.example.com"},
				}
			},
			wantErr: "token_env",
		},
		{
			name: "dedicated gitlab forge cleartext http on public host rejected",
			mutate: func(c *Config) {
				c.Actions.RevertPR.Forge = &RevertPRForge{
					Provider: "gitlab",
					GitLab: GitLab{
						BaseURL:  "http://gitlab.example.com",
						TokenEnv: "REVERT_GL_TOKEN",
					},
				}
			},
			wantErr: "cleartext",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseApprove()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
