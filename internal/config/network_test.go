// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNetworkProviderSelection covers the pluggable network-provider discriminator
// and the legacy `network: {url: ...}` back-compat mapping to provider=hubble.
func TestNetworkProviderSelection(t *testing.T) {
	tests := []struct {
		name         string
		yaml         string
		wantProvider string
		wantHubble   string
		wantAWSGroup string
		wantGCPProj  string
	}{
		{
			name:         "disabled by default (empty network)",
			yaml:         "gitops: {engine: flux}\n",
			wantProvider: "",
		},
		{
			name:         "hubble explicit",
			yaml:         "network:\n  provider: hubble\n  hubble:\n    url: hubble-relay.kube-system:80\n",
			wantProvider: NetworkHubble,
			wantHubble:   "hubble-relay.kube-system:80",
		},
		{
			name:         "legacy bare url maps to hubble",
			yaml:         "network:\n  url: hubble-relay.kube-system:80\n",
			wantProvider: NetworkHubble,
			wantHubble:   "hubble-relay.kube-system:80",
		},
		{
			name:         "aws vpc flow logs",
			yaml:         "network:\n  provider: aws-vpc-flow-logs\n  aws:\n    region: eu-west-3\n    log_group: /aws/vpc/flowlogs\n",
			wantProvider: NetworkAWSVPCFlowLogs,
			wantAWSGroup: "/aws/vpc/flowlogs",
		},
		{
			name:         "gcp firewall logs",
			yaml:         "network:\n  provider: gcp-firewall-logs\n  gcp:\n    project: my-project\n",
			wantProvider: NetworkGCPFirewallLogs,
			wantGCPProj:  "my-project",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tt.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.Network.Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", cfg.Network.Provider, tt.wantProvider)
			}
			if cfg.Network.Hubble.URL != tt.wantHubble {
				t.Errorf("hubble.url = %q, want %q", cfg.Network.Hubble.URL, tt.wantHubble)
			}
			if cfg.Network.AWS.LogGroup != tt.wantAWSGroup {
				t.Errorf("aws.log_group = %q, want %q", cfg.Network.AWS.LogGroup, tt.wantAWSGroup)
			}
			if cfg.Network.GCP.Project != tt.wantGCPProj {
				t.Errorf("gcp.project = %q, want %q", cfg.Network.GCP.Project, tt.wantGCPProj)
			}
		})
	}
}
