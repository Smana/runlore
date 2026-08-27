// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestGCPRejectsTheAWSOnlyFlatKeys pins a silent misconfiguration.
//
// cloud.region and cloud.cluster_name are flat for back-compatibility with every
// deployment that predates a second cloud; GCP nests. Nothing read the flat pair under
// provider=gcp and nothing said so — load.go uses KnownFields(true), so the block parses
// cleanly, the value lands in a field the GCP wiring never consults, and autodetection
// then either resolves a DIFFERENT cluster in the same project or leaves the scope
// unresolved. Either way the startup line looks correct.
//
// The same branch grew a warning for an unknown provider specifically so a deliberately
// set cloud key is never silent. This was the neighbouring silence.
func TestGCPRejectsTheAWSOnlyFlatKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cloud   Cloud
		wantErr bool
		wantSub string
	}{
		{
			name:    "cluster_name under gcp is rejected and names the nested key to use",
			cloud:   Cloud{Provider: CloudGCP, ClusterName: "my-gke"},
			wantErr: true, wantSub: "cloud.gcp.cluster_name",
		},
		{
			name:    "region under gcp is rejected and names the nested key to use",
			cloud:   Cloud{Provider: CloudGCP, Region: "europe-west1"},
			wantErr: true, wantSub: "cloud.gcp.location",
		},
		{
			name:  "the nested GCP block is accepted",
			cloud: Cloud{Provider: CloudGCP, GCP: GCPCloudCfg{Project: "p", Location: "l", ClusterName: "c"}},
		},
		{
			name:  "an empty GCP block is accepted, since everything autodetects",
			cloud: Cloud{Provider: CloudGCP},
		},
		{
			name:  "the flat pair is still correct for AWS, which is what it exists for",
			cloud: Cloud{Provider: CloudAWS, Region: "eu-west-3", ClusterName: "my-eks"},
		},
		{
			name:  "cloud context off is not validated at all",
			cloud: Cloud{ClusterName: "leftover"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCloud(tc.cloud)
			if tc.wantErr && err == nil {
				t.Fatal("a key that cannot take effect was accepted silently")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a valid cloud block was rejected: %v", err)
			}
			if tc.wantSub != "" && err != nil && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("the error does not name the key to move to (%q): %v", tc.wantSub, err)
			}
		})
	}
}
