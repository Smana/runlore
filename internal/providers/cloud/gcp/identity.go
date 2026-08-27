// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"cloud.google.com/go/compute/metadata"
)

// Metadata server keys, spelled as the paths under /computeMetadata/v1/ that the
// metadata client appends to its base URL.
//
// The first two exist on every Compute Engine instance. The two cluster attributes are
// GKE-specific: the control plane stamps them onto each node's instance metadata, and
// the GKE metadata server is what re-serves them to a Pod. Whether it re-serves
// cluster-location across every GKE version and mode is the open question this
// package's tier 3 exists to cover.
const (
	metaProjectID   = "project/project-id"
	metaProjectNum  = "project/numeric-project-id"
	metaClusterName = "instance/attributes/cluster-name"
	metaClusterLoc  = "instance/attributes/cluster-location"
)

// Tier names recorded in Identity.Source and printed in the startup log line.
//
// Source records the WEAKEST tier that contributed any part of the triple, not the
// strongest. That direction is the useful one: it answers "was tier N actually needed",
// which is precisely the question a live GKE run has to settle. A Source of
// "metadata-server" is proof that the node fallback contributed nothing on that
// cluster; "node-provider-id" is proof that it did.
const (
	sourceConfig   = "config"
	sourceMetadata = "metadata-server"
	sourceNone     = "unresolved"
)

// metadataSource is tier 2, behind an interface so the precedence table can be
// exercised without a metadata server and without the multi-second probe the real
// client performs off-GCE.
type metadataSource interface {
	metadataGet(ctx context.Context, key string) (string, error)
}

// errNoMetadataServer reports that this process is not on Compute Engine, so tier 2 has
// nothing to ask.
var errNoMetadataServer = errors.New("gcp: not running on Compute Engine")

// liveMetadata is the production tier-2 source.
type liveMetadata struct{}

// metadataGet reads one key, refusing outright when the process is not on Compute
// Engine.
//
// The OnGCE guard is not an optimisation, it is what keeps a non-GCP deployment from
// stalling at startup. cloud.google.com/go/compute/metadata dials the link-local
// 169.254.169.254 with a 2-second dialer timeout, and its retryer treats a dial timeout
// as transient (net.Error's Temporary() is true for timeouts) and retries up to five
// times with exponential backoff — on the order of fifteen seconds for a single key
// against an address that simply never answers, which this resolver would then pay four
// times over. OnGCE costs one probe, bounded by the same 2-second dial, and the package
// memoises its result for the life of the process.
//
// It is also the correct answer rather than merely the fast one on a foreign cloud: an
// EC2 node answers on that same link-local address, and refusing before the request is
// how this provider avoids reading an AWS instance-metadata response as if it were a
// GCP project id. GCE_METADATA_HOST overrides both the probe and the destination, which
// is the documented way to point the client at a stand-in.
func (liveMetadata) metadataGet(ctx context.Context, key string) (string, error) {
	if !metadata.OnGCEWithContext(ctx) {
		return "", errNoMetadataServer
	}
	return metadata.GetWithContext(ctx, key)
}

// resolveIdentity fills the triple from the first tier that answers each field:
// explicit config, then the GKE metadata server, then a cluster node.
//
// Resolution is per FIELD, not per tier, and that is a deliberate difference from a
// simpler "first tier that answers anything wins". The attribute most likely to be
// missing is the cluster name, and an operator who supplies just that one should not
// thereby have to restate the project and location the metadata server already knows —
// restating them by hand is how a deployment ends up pointed at last quarter's cluster
// after a rebuild.
//
// Nothing here fails. An unresolved identity is returned with Source == sourceNone and
// an empty Project, and New refuses it with a message naming the config key to set;
// spreading that refusal across both would produce two different errors for one cause.
func resolveIdentity(ctx context.Context, cfg Identity, meta metadataSource, nodes NodeLookup) Identity {
	out := cfg

	// Tier 1. Config counts as the source only for the three fields an operator can
	// actually set — ProjectNumber is resolved below and is deliberately not
	// configurable.
	source := ""
	if out.Project != "" || out.Location != "" || out.ClusterName != "" {
		source = sourceConfig
	}

	// A key the cluster does not stamp comes back as a 404-derived error, which is
	// ordinary here rather than exceptional, so the error collapses to "". TrimSpace
	// because the server returns some values with surrounding whitespace — the metadata
	// package trims project-id and numeric-project-id in its own accessors for the same
	// reason, and an untrimmed project id silently produces "projects/acme-prod\n".
	get := func(key string) string {
		v, err := meta.metadataGet(ctx, key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}

	// Tier 2. Three independent reads rather than one all-or-nothing fetch, because
	// the attributes fail independently: a cluster whose metadata server stamps
	// cluster-name but not cluster-location should still yield the name.
	if out.Project == "" {
		if v := get(metaProjectID); v != "" {
			out.Project, source = v, sourceMetadata
		}
	}
	if out.ClusterName == "" {
		if v := get(metaClusterName); v != "" {
			out.ClusterName, source = v, sourceMetadata
		}
	}
	if out.Location == "" {
		if v := get(metaClusterLoc); v != "" {
			out.Location, source = v, sourceMetadata
		}
	}

	// The project NUMBER is never configurable and never affects Source. It is used
	// only to render the principal:// binding hint on a permission denial — IAM
	// principal strings are written with the number and reject the id — so an
	// operator-supplied one would be a new way to print a confidently wrong command,
	// and its absence degrades a diagnostic rather than a query.
	if out.ProjectNumber == "" {
		out.ProjectNumber = get(metaProjectNum)
	}

	// Tier 3 — see the section at the bottom of this file. Deleting it is this one
	// statement plus that section plus the nodes parameter.
	if applyNodeTier(ctx, &out, nodes) {
		source = sourceNode
	}

	if source == "" {
		source = sourceNone
	}
	out.Source = source
	return out
}

// ResolveIdentity resolves the GCP scope for this process and logs what it found.
//
// cfg carries whatever cloud.gcp.* stated; nodes is an optional reader for a cluster
// node, and may be nil. log is the application's logger and must not be nil.
//
// The logger is a parameter rather than the package-level slog default, which is what
// this used. Nothing in this repo calls slog.SetDefault — serve builds its own logger
// and the chart defaults to JSON on stdout — so those lines went to Go's default
// handler instead: plain text on stderr, at the default level, outside the pipeline
// every neighbouring line lands in. That made the one field an operator is told to grep
// for (`source`, to settle which tier resolved the scope) invisible to the query the
// docs prescribe.
//
// The line is emitted here rather than in the resolver so that the resolver stays a pure
// function of its inputs, and because this is the once-per-process call: RunLore
// resolves the scope during wiring and then reuses it for every investigation, so one
// line at startup is a complete record. It reports the tier alongside the triple
// because the triple alone cannot be checked. Autodetection landing on a neighbouring
// cluster in the same project produces answers that are confidently about the wrong
// cluster and indistinguishable from a quiet one, and the tier is the only part of the
// line that says where the scope came from.
func ResolveIdentity(ctx context.Context, cfg Identity, nodes NodeLookup, log *slog.Logger) Identity {
	id := resolveIdentity(ctx, cfg, liveMetadata{}, nodes)

	attrs := []any{
		"project", id.Project,
		"location", id.Location,
		"cluster", id.ClusterName,
		"source", id.Source,
	}
	if id.Source == sourceNone {
		// Warn rather than error: New is what refuses, and it names the one key that
		// is strictly required. This line names all three because an operator reading
		// it has to set the ones autodetection missed, and being told about
		// cloud.gcp.project alone would earn a second restart for the location.
		log.Warn("gcp: could not resolve project, location or cluster; "+
			"set cloud.gcp.project, cloud.gcp.location and cloud.gcp.cluster_name", attrs...)
		return id
	}
	log.Info("gcp: resolved cloud identity", attrs...)
	return id
}

// ---------------------------------------------------------------------------
// TIER 3 — PROVISIONAL. Everything below this line is deletable as one block.
//
// It exists only because it is not established that the GKE metadata server proxies
// instance/attributes/cluster-location to Pods across every GKE version and mode, and
// the zero-configuration promise should not rest on an untested assumption. A live GKE
// startup log line reporting source=metadata-server settles that; source=node-provider-id
// says this tier earned its place.
//
// It is WIRED: internal/app passes a NodeLookup backed by the cluster reader, so that
// log line can actually be produced. It was briefly not, and the difference matters —
// with the argument hardwired to nil the tier could never contribute, so the experiment
// this block exists to run could never return a positive result, and the removal
// criterion below could never be met either way.
//
// To remove it once tier 2 is proven, three edits, all of which the compiler will point
// at:
//
//  1. Delete everything below this banner.
//  2. Delete the applyNodeTier call in resolveIdentity.
//  3. Drop the nodes parameter from resolveIdentity and ResolveIdentity, and the
//     argument wherever ResolveIdentity is called — then drop the "nodes" RBAC rule
//     from the chart's ClusterRole, which exists only for this tier.
//
// Above the banner, tier 3 appears only as that parameter and that statement. No tier-1
// or tier-2 behaviour changes when it goes, because the fields it fills are the fields
// tier 2 fills, by the same per-field rule — removing it can only turn a resolved field
// back into an empty one, which New already refuses with a message naming the config key.
// ---------------------------------------------------------------------------

// sourceNode marks an identity that needed the node fallback.
const sourceNode = "node-provider-id"

// NodeIdentity is the raw slice of a Kubernetes node object tier 3 reads: the two
// fields, and only those, that can locate a GKE cluster.
//
// Raw rather than pre-resolved on purpose. If the caller handed over a finished
// (project, location) pair it would have to know that a GKE providerID is
// gce://PROJECT/ZONE/INSTANCE and that a region label outranks that zone — leaving
// tier-3 knowledge in internal/app that would be orphaned, not deleted, when this
// section goes.
type NodeIdentity struct {
	// ProviderID is .spec.providerID, "gce://PROJECT/ZONE/INSTANCE" on a GKE node.
	ProviderID string
	// Region is the topology.kubernetes.io/region label. Optional; a node that has
	// somehow lost it still yields a location from its providerID's zone.
	Region string
}

// NodeLookup reads one node from the cluster. A nil NodeLookup, or one that returns an
// error, makes tier 3 unavailable rather than fatal — RunLore may be deployed with RBAC
// that does not list nodes, and that must cost a fallback, not a startup.
type NodeLookup func(ctx context.Context) (NodeIdentity, error)

// applyNodeTier fills whatever of project and location is still missing from a cluster
// node, and reports whether it contributed anything.
//
// It never supplies the cluster NAME. A node carries no attribute that names its
// cluster in a form the container API accepts, and guessing one from a node-pool name
// would produce a describe against a cluster that does not exist.
//
// The location it produces prefers the region label over the providerID's zone, which
// is right for a regional cluster and wrong for a zonal one — a node cannot tell the
// two apart. Region is still the better default because it is the same on every node of
// a regional cluster, where the zone is not: resolving from whichever node the lookup
// happened to return would make the scope differ across restarts, and a scope that
// moves is worse to diagnose than one that is consistently wrong. Either way the
// mistake is loud — clusters.get 404s on a location that has no such cluster, naming
// the location it tried — and cloud.gcp.location is the fix.
func applyNodeTier(ctx context.Context, out *Identity, nodes NodeLookup) bool {
	if nodes == nil || (out.Project != "" && out.Location != "") {
		return false
	}
	n, err := nodes(ctx)
	if err != nil {
		return false
	}
	project, zone, ok := parseProviderID(n.ProviderID)
	if !ok {
		// Not a GCE node. Its labels are then not GCP topology either, so the region
		// label is dropped with the rest rather than trusted on its own.
		return false
	}

	used := false
	if out.Project == "" {
		out.Project, used = project, true
	}
	if out.Location == "" {
		if n.Region != "" {
			out.Location, used = n.Region, true
		} else {
			out.Location, used = zone, true
		}
	}
	return used
}

// parseProviderID extracts (project, zone) from a GKE node's spec.providerID, which is
// "gce://PROJECT/ZONE/INSTANCE".
//
// It returns ok=false for every other shape — an AWS or bare-metal providerID, a
// truncated one, an empty project segment — because a partial parse is far worse here
// than no parse at all. Every Cloud Logging, container and compute call this provider
// makes is addressed to "projects/<id>/…", so a plausible-looking wrong project scopes
// an entire investigation to the wrong place and returns a confident empty answer,
// whereas a rejection surfaces at startup as a message naming the key to set.
func parseProviderID(id string) (project, zone string, ok bool) {
	const prefix = "gce://"
	rest, found := strings.CutPrefix(id, prefix)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
