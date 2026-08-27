// SPDX-License-Identifier: Apache-2.0

// Package gcp implements providers.CloudProvider against GCP using the Google API Go
// clients and in-cluster identity. All calls are read-only: Cloud Logging entries.list
// over the audit streams (the GCP "what changed" lens), and container/compute describes
// (resource health).
//
// Auth is Application Default Credentials, which on GKE resolves to Workload Identity.
// The intended binding is a DIRECT PRINCIPAL binding — the Kubernetes ServiceAccount is
// granted the IAM roles itself, with no intermediate Google service account and no
// iam.gke.io/gcp-service-account annotation on the KSA. That shape needs no code here;
// ADC finds the credential either way. What it does need is diagnosis, because a
// missing or misspelled binding surfaces only as a bare 403 from whichever call runs
// first, in the middle of an investigation, with nothing naming the principal that was
// actually presented.
//
// The Google generated clients are structs rather than interfaces, so unlike the AWS
// provider — whose SDK clients are swapped wholesale for fakes — the seam here is the
// ClientOption list threaded through New. Tests inject option.WithHTTPClient +
// option.WithEndpoint + option.WithoutAuthentication and serve canned JSON from
// httptest, the pattern internal/network/gcpfirewall already uses against the same
// Cloud Logging API.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/gcplog"
	"github.com/Smana/runlore/internal/providers"
)

// defaultMaxEvents is the shared cross-cloud budget; see providers.DefaultCloudMaxEvents
// for why it is not a per-provider choice.
const defaultMaxEvents = providers.DefaultCloudMaxEvents

// Identity is the resolved GCP scope every call in this package is addressed to.
//
// It is a plain value rather than something the Client resolves for itself because the
// resolution has several tiers (explicit config, the GKE metadata server, the
// Kubernetes node providerID) with quite different failure modes and testing needs,
// and folding them into the constructor would make every Client test also a test of
// metadata-server behaviour. The resolver that produces one of these lives in
// identity.go; New only validates what it is handed.
type Identity struct {
	Project     string // project id, e.g. "acme-prod" — the "projects/<id>" every request is scoped to
	Location    string // the cluster's region or zone; GKE treats both as a "location"
	ClusterName string // GKE cluster name

	// ProjectNumber is the numeric form of Project. Both identify the same project, but
	// IAM principal:// strings are written with the NUMBER and nothing accepts the id
	// there, so a diagnostic that suggests a binding has to have resolved it. Optional:
	// its absence degrades a hint, not a query.
	ProjectNumber string

	// PodNamespace and PodServiceAccount name the Kubernetes ServiceAccount this
	// process runs as, used only to render the principal:// binding hint on a denial.
	//
	// Supplied rather than read from the environment here, for the reason the rest of
	// this struct is supplied: internal/app already owns the question "who is this pod"
	// and answers it better (PodNamespace falls back to the service-account mount, which
	// exists in every pod even when the chart's downward-API value does not). A second
	// copy inside the provider answered it worse and could not be injected by a test.
	// Either may be empty; the renderer substitutes a visible placeholder.
	PodNamespace      string
	PodServiceAccount string

	// Source names which tier resolved this triple, for the line logged at startup.
	// Worth carrying because the tiers disagree silently: autodetection can land on a
	// neighbouring cluster in the same project and every subsequent answer is then
	// confidently about the wrong cluster, which is indistinguishable from a quiet one
	// unless the operator can see where the scope came from.
	Source string
}

// Client is the GCP cloud provider.
type Client struct {
	entries   gcplog.EntriesAPI
	container *container.Service
	compute   *compute.Service

	// id is held whole rather than copied out field by field. The flat copy meant
	// adding an Identity field required editing the struct, this struct and New, with
	// the compiler flagging none of them, and it renamed Source to identitySrc so a
	// reader had to map two vocabularies for one value.
	id Identity

	// maxEvents is int, matching cloud/aws's field, so a reader moving between the two
	// providers is not re-learning the type. The one place a wire type needs int64
	// converts there.
	maxEvents int
}

// New builds a Client from Application Default Credentials.
//
// opts are passed to every service constructor, which is the whole testing seam for
// this package: production passes none and gets ADC, tests pass
// option.WithHTTPClient + option.WithEndpoint + option.WithoutAuthentication. Passing
// them to all three services matters more than it looks — a constructor that misses the
// list silently keeps its generated googleapis.com BasePath, so two thirds of a test
// suite can pass while one lens quietly tries to reach the internet.
func New(ctx context.Context, id Identity, opts ...option.ClientOption) (*Client, error) {
	// Checked here rather than left to the API: every request this provider makes is
	// addressed to "projects/<id>/…", so an empty project yields a client that builds
	// fine and then 400s on first use, several layers away from the actual cause. The
	// message names the config key because the fallback for failed autodetection is for
	// an operator to set it explicitly.
	if id.Project == "" {
		return nil, errors.New("gcp: project is required (autodetection found none; set cloud.gcp.project)")
	}
	lsvc, err := logging.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new logging service: %w", err)
	}
	csvc, err := container.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new container service: %w", err)
	}
	msvc, err := compute.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new compute service: %w", err)
	}
	return &Client{
		entries:   gcplog.Entries(lsvc),
		container: csvc,
		compute:   msvc,
		id:        id,
		maxEvents: defaultMaxEvents,
	}, nil
}

var (
	_ providers.CloudProvider    = (*Client)(nil)
	_ providers.CloudDescriber   = (*Client)(nil)
	_ providers.CloudPreflighter = (*Client)(nil)
)

// CloudVocabulary describes Cloud Audit Logs and GKE, so the cloud tools never tell a
// GCP deployment's model that it is reading CloudTrail.
//
// Two fragments differ from AWS in substance rather than just nouns, and both change
// what a model should DO:
//
// ScopeGuidance says SUBSTRING, because a Cloud Logging filter written with the ':'
// operator matches any part of protoPayload.resourceName, where CloudTrail's
// ResourceName is an exact match. On AWS a half-remembered identifier is worthless and
// the right move is to omit it; on GCP it is often enough on its own. Getting this
// backwards is the exact class of wrong belief that dead-ended the investigation
// recorded in internal/investigate/cloud_tools.go.
//
// LagNote says well under a minute, against CloudTrail's ~15m. A GCP investigation
// therefore does not need to over-widen its window to see a change that just happened,
// and a genuinely empty recent window is real evidence rather than an artifact of
// ingestion delay.
//
// FailureFilterArg rests on a third difference. On both clouds this tool filters reads
// out, but on GCP the reads are usually not in the log at all: Admin Activity audit
// logs are always on and cannot be disabled, while Data Access audit logs are off by
// default for every service except BigQuery. A model handed the AWS sentence would be
// hunting for "Describe" and "Get" — AWS's read verbs — in a log whose entries name
// google.container.v1.ClusterManager.UpdateCluster and the like.
func (Client) CloudVocabulary() providers.CloudVocabulary {
	return providers.CloudVocabulary{
		Engine:   providers.EngineGCP,
		Cloud:    "GCP",
		AuditLog: "Cloud Audit Logs",
		ChangeExamples: "GKE/Compute/IAM/network changes, manual actions, Google-initiated host events " +
			"(host error, live migration, preemption)",
		TimelineExamples: "GKE/Compute/IAM/manual actions",
		ScopeGuidance: "Optional resource is a SUBSTRING match on protoPayload.resourceName — a bare " +
			"name like \"my-nodepool\" matches, and so does a full " +
			"\"projects/p/zones/z/instances/my-vm\" path;",
		FailureFilterNote: "Set failed_only=true when the incident IS a rejected GCP call and you do not " +
			"know which resource it happened to (a node pool that would not scale out, a disk attach " +
			"that was denied): results are capped at the NEWEST entries, which on a busy project are " +
			"routine instance and metadata churn, so the rejected call you are looking for is usually " +
			"just past the cap. failed_only spends the cap on rejected calls instead and reports each " +
			"one's status.",
		FailureFilterArg: "keep only MUTATING control-plane calls that were REJECTED, reporting each " +
			"status; use when the incident is itself a rejected GCP write. Read-only calls are never " +
			"listed by this tool, and Data Access audit logs are off by default outside BigQuery, so a " +
			"denied get/list will NOT appear here",
		WidenedBanner: func(resource string) string {
			return fmt.Sprintf("resource %q matched no Cloud Audit Log entries in the window. The "+
				"filter is a SUBSTRING match on protoPayload.resourceName, so a partial identifier "+
				"would have been enough — nothing in the window carried that string at all. Showing "+
				"ALL mutating entries in the window instead:\n", resource)
		},
		LagNote: "Cloud Audit Logs lag well under a minute",
		HealthSurface: "GKE cluster and node-pool status + conditions, managed-instance-group errors " +
			"(stockouts, quota and IP exhaustion), and — when given a Compute Engine instance name — " +
			"its instance status.",
		InstanceArg: "optional Compute Engine instance name",
		// GCP has no "scaling activities"; the skeleton used to say so anyway. What
		// since_minutes actually scopes here is instance-group ERROR filtering
		// (errorInWindow in resourcehealth.go).
		HealthWindowNote: "scopes the instance-group-error lookback to the incident window " +
			"(default: recent errors).",
		HealthWindowArg: "scope instance-group-error lookback to the last N minutes",
	}
}

// Preflight makes one cheap authoritative read to confirm the Workload Identity
// binding actually grants what the cloud lens needs, so a misconfiguration surfaces at
// startup with a fix attached rather than as a bare 403 partway through an
// investigation.
//
// With direct principal binding, ADC resolves credentials with no RunLore code at all —
// so what was missing was never the wiring, it was the diagnosis. The failure this
// catches is silent by construction: the pod starts, the tools register, and the first
// investigation that reaches for the cloud lens gets a permission error naming an API
// rather than the binding nobody created.
//
// It probes ONLY Cloud Logging — the changes lens, which is the core of the provider.
// The health APIs degrade per-sub-query at call time (resourcehealth.go) and produce a
// role-specific diagnosis in place, so probing them here would buy three extra startup
// calls for an answer that already arrives where it is needed.
func (c *Client) Preflight(ctx context.Context) error {
	_, err := c.entries.List(ctx, &logging.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.id.Project},
		Filter:        fmt.Sprintf(`logName="projects/%s/logs/%s"`, c.id.Project, activityLog),
		PageSize:      1,
	})
	if err == nil {
		return nil
	}
	if !deniedStatus(err) {
		// Deliberately NOT wrapped as denied. This is a 503, a DNS failure or a
		// deadline, and the caller must keep the lens registered for it — wiring runs
		// once per process, so treating a startup blip as permanent would leave the
		// cloud tools absent until someone restarts the pod.
		return fmt.Errorf("cloud logging read failed on project %s: %w", c.id.Project, err)
	}
	// Lowercase and no trailing period: staticcheck ST1005 and revive's error-strings
	// are both in this repo's gate, and neither exempts "Cloud".
	return &DeniedError{
		Summary: fmt.Sprintf("cloud logging read denied on project %s: RunLore authenticated as "+
			"ServiceAccount %s/%s, which no GCP role is bound to",
			c.id.Project, c.podNamespace(), c.podServiceAccount()),
		Command: fmt.Sprintf("gcloud projects add-iam-policy-binding %s \\\n"+
			"    --role=roles/logging.viewer \\\n"+
			"    --member=%q\n\n"+
			"repeat for roles/container.clusterViewer and roles/compute.viewer; "+
			"note the project NUMBER in the member string — the project ID does not work there",
			c.id.Project, c.principal()),
	}
}

// DeniedError is a preflight authorization failure, with the human summary and the
// pastable remediation command kept as SEPARATE fields.
//
// Separate because of how the two are consumed. The summary belongs in a structured log
// line; the command must survive being read by a human, and the chart defaults to JSON
// logging, where a multi-line value embedded in a message arrives as one
// escape-sequence-laden string with "\n" and "\\" written out literally. That is
// unpastable, which removes the only reason to print the command at all. Splitting them
// lets the caller log the summary and write the command where it stays copyable.
//
// Error() still renders both, so a caller that only has %v loses nothing.
type DeniedError struct {
	Summary string
	Command string
}

func (e *DeniedError) Error() string { return e.Summary + "\n\n  " + e.Command }

// Unwrap reports this as the permanent, authorization kind of preflight failure, so
// providers.CloudPreflightDenied answers true for it and only for it.
func (e *DeniedError) Unwrap() error { return providers.ErrCloudPreflightDenied }

// deniedStatus reports whether err is an authorization failure rather than any other
// API error. 401 counts alongside 403 because a pod with no usable ADC at all fails
// that way, and the binding command is the right answer to both.
func deniedStatus(err error) bool {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	return gerr.Code == http.StatusForbidden || gerr.Code == http.StatusUnauthorized
}

// principal renders the Workload Identity direct-binding member string for this pod's
// Kubernetes ServiceAccount.
//
// It uses the numeric project id, which is NOT interchangeable with the project id
// here: a member string built with the id is accepted by gcloud and then silently
// never matches. That is the single most common way a direct binding is set up wrong,
// and the reason this command is generated rather than left to the documentation.
func (c *Client) principal() string {
	// Same placeholder discipline as podNamespace/podServiceAccount, and for a sharper
	// reason: projectNum comes only from the metadata server, so an operator using the
	// explicit-config escape hatch on a pod that cannot reach it would otherwise render
	// "projects//locations/…" — which gcloud accepts and which never matches. An
	// obviously-a-template placeholder is the one thing worse than no command that is
	// still better than a silently wrong one.
	num := orPlaceholder(c.id.ProjectNumber, "<project-number>")
	return fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
		num, c.id.Project, c.podNamespace(), c.podServiceAccount())
}

// podNamespace and podServiceAccount render this pod's identity for the binding hint,
// falling back to a placeholder rather than an empty string so a command printed
// outside a pod is visibly a template instead of a subtly wrong binding that looks
// complete.
//
// The values are resolved by the caller (see Identity); these only decide what an
// absent one looks like.
func (c *Client) podNamespace() string {
	return orPlaceholder(c.id.PodNamespace, "<namespace>")
}

func (c *Client) podServiceAccount() string {
	return orPlaceholder(c.id.PodServiceAccount, "<serviceaccount>")
}

// orPlaceholder returns v, or an obviously-a-template stand-in when v is empty.
func orPlaceholder(v, placeholder string) string {
	if v == "" {
		return placeholder
	}
	return v
}
