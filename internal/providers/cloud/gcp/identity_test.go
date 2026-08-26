// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubMetadata answers from a fixed map, so the precedence table is observable without
// a metadata server.
//
// An absent key errors rather than returning empty, because that is what the real
// server does: an attribute a cluster does not stamp comes back 404, which the metadata
// client turns into NotDefinedError. A stub that returned ("", nil) would let a resolver
// that ignores the error still pass every case here.
type stubMetadata map[string]string

func (s stubMetadata) metadataGet(_ context.Context, key string) (string, error) {
	if v, ok := s[key]; ok && v != "" {
		return v, nil
	}
	return "", errors.New("metadata: not defined: " + key)
}

// stubNode is a NodeLookup that always answers with the same node.
func stubNode(n NodeIdentity) NodeLookup {
	return func(context.Context) (NodeIdentity, error) { return n, nil }
}

// failingNode is a NodeLookup that cannot read the cluster — the shape of a RunLore
// whose RBAC does not include nodes, which must degrade rather than abort resolution.
func failingNode() NodeLookup {
	return func(context.Context) (NodeIdentity, error) {
		return NodeIdentity{}, errors.New("nodes is forbidden")
	}
}

// fullMetadata is a GKE metadata server that stamps every attribute.
func fullMetadata() stubMetadata {
	return stubMetadata{
		metaProjectID:   "meta-proj",
		metaProjectNum:  "123456789012",
		metaClusterName: "meta-cluster",
		metaClusterLoc:  "europe-west1",
	}
}

// TestResolveIdentityPrefersConfigThenMetadataThenTheNodeObject pins the precedence the
// whole zero-configuration promise rests on, per field rather than per tier.
//
// Per field is the part worth a test: an operator who sets only cloud.gcp.cluster_name
// — the one attribute a metadata server may not stamp — must not thereby have to
// restate the project and location it does stamp. A per-tier resolver would pass a test
// that only ever supplied all three or none.
//
// Source is asserted on every case because it is not decoration. It records the WEAKEST
// tier that contributed, which is the single observation that decides whether tier 3
// survives: on a real GKE cluster, "metadata-server" means tier 3 was never needed and
// "node-provider-id" means it was.
func TestResolveIdentityPrefersConfigThenMetadataThenTheNodeObject(t *testing.T) {
	const gkeNode = "gce://node-proj/us-central1-a/gke-prod-pool-a1b2c3"

	cases := []struct {
		name  string
		cfg   Identity
		meta  stubMetadata
		nodes NodeLookup
		want  Identity
	}{
		{
			name:  "explicit config wins outright — it is the operator overriding detection",
			cfg:   Identity{Project: "cfg-proj", Location: "asia-east1", ClusterName: "cfg-cluster"},
			meta:  fullMetadata(),
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode, Region: "us-central1"}),
			want: Identity{
				Project: "cfg-proj", Location: "asia-east1", ClusterName: "cfg-cluster",
				ProjectNumber: "123456789012", Source: sourceConfig,
			},
		},
		{
			name:  "no config: the metadata server answers all three",
			meta:  fullMetadata(),
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode, Region: "us-central1"}),
			want: Identity{
				Project: "meta-proj", Location: "europe-west1", ClusterName: "meta-cluster",
				ProjectNumber: "123456789012", Source: sourceMetadata,
			},
		},
		{
			name:  "config fills only what it states; the rest still falls through to metadata",
			cfg:   Identity{ClusterName: "cfg-cluster"},
			meta:  fullMetadata(),
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode, Region: "us-central1"}),
			want: Identity{
				Project: "meta-proj", Location: "europe-west1", ClusterName: "cfg-cluster",
				ProjectNumber: "123456789012", Source: sourceMetadata,
			},
		},
		{
			name:  "metadata lacks the cluster attributes: the node supplies the location, and the cluster name stays empty",
			meta:  stubMetadata{metaProjectID: "meta-proj", metaProjectNum: "123456789012"},
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode, Region: "us-central1"}),
			want: Identity{
				Project: "meta-proj", Location: "us-central1", ClusterName: "",
				ProjectNumber: "123456789012", Source: sourceNode,
			},
		},
		{
			name:  "no metadata server at all: the node supplies project and location",
			meta:  stubMetadata{},
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode, Region: "us-central1"}),
			want:  Identity{Project: "node-proj", Location: "us-central1", Source: sourceNode},
		},
		{
			name:  "a node with no region label falls back to the zone in its providerID",
			meta:  stubMetadata{},
			nodes: stubNode(NodeIdentity{ProviderID: gkeNode}),
			want:  Identity{Project: "node-proj", Location: "us-central1-a", Source: sourceNode},
		},
		{
			name:  "a node that is not a GCE node contributes nothing, not even its region label",
			meta:  stubMetadata{},
			nodes: stubNode(NodeIdentity{ProviderID: "aws:///eu-west-1a/i-0abc", Region: "eu-west-1"}),
			want:  Identity{Source: sourceNone},
		},
		{
			name:  "a node lookup that is refused leaves the earlier tiers' answer intact",
			meta:  stubMetadata{metaProjectID: "meta-proj"},
			nodes: failingNode(),
			want:  Identity{Project: "meta-proj", Source: sourceMetadata},
		},
		{
			name: "no node lookup wired: tier 3 is simply unavailable",
			meta: stubMetadata{metaProjectID: "meta-proj"},
			want: Identity{Project: "meta-proj", Source: sourceMetadata},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveIdentity(context.Background(), c.cfg, c.meta, c.nodes)
			if got != c.want {
				t.Errorf("resolveIdentity\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestAnUnresolvedIdentityIsRefusedByNewNamingTheConfigKeyToSet joins the resolver to
// the constructor, which is where "nothing answered" has to become an operator-readable
// error.
//
// Neither half proves it alone. The resolver deliberately does not fail — it reports
// sourceNone and an empty project — and New's own test hands it a hand-written Identity.
// The failure this covers is the seam: a resolver that invented a placeholder project,
// or a Source that read "metadata-server" after the metadata server answered nothing,
// would leave New building a client that 400s on "projects/" mid-investigation with the
// real cause several layers away.
func TestAnUnresolvedIdentityIsRefusedByNewNamingTheConfigKeyToSet(t *testing.T) {
	id := resolveIdentity(context.Background(), Identity{}, stubMetadata{}, nil)
	if id.Project != "" {
		t.Errorf("Project = %q, want empty — no tier answered", id.Project)
	}
	if id.Source != sourceNone {
		t.Errorf("Source = %q, want %q", id.Source, sourceNone)
	}

	_, err := New(context.Background(), id)
	if err == nil {
		t.Fatal("New accepted an unresolved identity")
	}
	if !strings.Contains(err.Error(), "cloud.gcp.project") {
		t.Errorf("the error does not name the config key that fixes it: %v", err)
	}
}

// TestParseProviderIDAcceptsOnlyAGKEProviderID guards the one string tier 3 parses.
//
// A partial parse is the dangerous outcome, not a rejected one: every Cloud Logging,
// container and compute call is addressed to "projects/<id>/…", so a providerID that
// yielded a plausible-looking wrong project would scope an entire investigation to
// somebody else's cluster and answer confidently. A rejection costs an unresolved
// identity and a startup error naming the config key to set.
func TestParseProviderIDAcceptsOnlyAGKEProviderID(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		project string
		zone    string
		ok      bool
	}{
		{"a GKE node", "gce://my-proj/us-central1-a/gke-prod-pool-abc", "my-proj", "us-central1-a", true},
		{"an AWS node is not ours", "aws:///eu-west-1a/i-0abc", "", "", false},
		{"a truncated id yields nothing", "gce://my-proj", "", "", false},
		{"a project-and-zone-only id has no instance and is not the documented shape", "gce://my-proj/us-central1-a", "", "", false},
		{"an empty project segment is rejected rather than scoping to \"projects/\"", "gce:///us-central1-a/inst", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, z, ok := parseProviderID(c.in)
			if ok != c.ok || p != c.project || z != c.zone {
				t.Errorf("parseProviderID(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.in, p, z, ok, c.project, c.zone, c.ok)
			}
		})
	}
}

// metadataServer starts an httptest server that answers /computeMetadata/v1/<key> from
// keys and 404s everything else, and points the metadata client at it for the duration
// of the test.
//
// GCE_METADATA_HOST is the override cloud.google.com/go/compute/metadata documents for
// exactly this ("to enable spoofing of the metadata service"), and it is also what makes
// OnGCE return true without a probe — so a test using it exercises the real client, the
// real URL shape and the real 404-to-error mapping, and still cannot touch
// 169.254.169.254.
//
// EVERY test in this package that reaches liveMetadata must go through this helper. The
// metadata package memoises OnGCE in a sync.Once for the life of the process, so one
// test calling liveMetadata without the variable set would pin it to false and every
// later test here would silently resolve nothing.
func metadataServer(t *testing.T, keys map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			// The real server refuses requests without it; so must the fake, or a
			// client that stopped sending it would go unnoticed.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/computeMetadata/v1/")
		v, ok := keys[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = w.Write([]byte(v))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
}

// TestTheLiveMetadataSourceReadsTheDocumentedGKEAttributePaths proves the production
// tier-2 source against a real metadata client, which the stub-driven precedence table
// cannot.
//
// The precedence table would pass identically if every metaXxx constant held a typo:
// the stub is keyed by the same constants. Only a request that has to travel the real
// "http://<host>/computeMetadata/v1/<key>" URL shape catches "instance/attribute/…" or
// "project/numeric-project-number".
func TestTheLiveMetadataSourceReadsTheDocumentedGKEAttributePaths(t *testing.T) {
	metadataServer(t, map[string]string{
		"project/project-id":                   "live-proj",
		"project/numeric-project-id":           "987654321098",
		"instance/attributes/cluster-name":     "live-cluster",
		"instance/attributes/cluster-location": "europe-west9",
	})

	got := resolveIdentity(context.Background(), Identity{}, liveMetadata{}, nil)
	want := Identity{
		Project: "live-proj", Location: "europe-west9", ClusterName: "live-cluster",
		ProjectNumber: "987654321098", Source: sourceMetadata,
	}
	if got != want {
		t.Errorf("resolveIdentity against a real metadata client\n got %+v\nwant %+v", got, want)
	}
}

// captureLogs redirects slog's default logger into a buffer for the duration of the
// test. ResolveIdentity logs through the default logger because it runs during app
// wiring, before any component-scoped logger exists.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// TestResolveIdentityLogsTheResolvedTripleAndTheTierThatProducedIt covers the one
// output of this package that is read by a human rather than a model.
//
// The tier is the load-bearing half. Tier 3 is provisional, and the only way to decide
// whether it survives is to look at a real GKE cluster's startup line and see which tier
// answered — a line that printed the triple alone would leave that question open for as
// long as the deployment runs.
func TestResolveIdentityLogsTheResolvedTripleAndTheTierThatProducedIt(t *testing.T) {
	t.Run("a resolved identity logs the triple and its tier", func(t *testing.T) {
		metadataServer(t, map[string]string{
			"project/project-id":                   "log-proj",
			"instance/attributes/cluster-name":     "log-cluster",
			"instance/attributes/cluster-location": "europe-west1",
		})
		buf := captureLogs(t)

		ResolveIdentity(context.Background(), Identity{}, nil)

		out := buf.String()
		for _, want := range []string{"log-proj", "europe-west1", "log-cluster", sourceMetadata} {
			if !strings.Contains(out, want) {
				t.Errorf("startup log line does not report %q:\n%s", want, out)
			}
		}
		if strings.Count(out, "\n") != 1 {
			t.Errorf("want exactly one startup log line, got:\n%s", out)
		}
	})

	t.Run("an unresolved identity warns and names every key that would fix it", func(t *testing.T) {
		metadataServer(t, nil)
		buf := captureLogs(t)

		ResolveIdentity(context.Background(), Identity{}, nil)

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("an unresolved identity was not logged at WARN:\n%s", out)
		}
		for _, want := range []string{"cloud.gcp.project", "cloud.gcp.location", "cloud.gcp.cluster_name", sourceNone} {
			if !strings.Contains(out, want) {
				t.Errorf("the warning does not mention %q:\n%s", want, out)
			}
		}
	})
}
