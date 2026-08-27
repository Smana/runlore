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
	"os"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// defaultMaxEvents bounds how many entries a single lens returns. Deliberately the same
// budget as the AWS client, so neither cloud floods a model's context where the other
// would not — an investigation's conclusions should not depend on which cloud it ran
// against.
const defaultMaxEvents = 25

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

	// Source names which tier resolved this triple, for the line logged at startup.
	// Worth carrying because the tiers disagree silently: autodetection can land on a
	// neighbouring cluster in the same project and every subsequent answer is then
	// confidently about the wrong cluster, which is indistinguishable from a quiet one
	// unless the operator can see where the scope came from.
	Source string
}

// entriesAPI is the narrow slice of Cloud Logging this provider uses.
//
// One method, because that is genuinely all the audit lens needs, and because a
// narrower interface is what lets a test assert on the request that was built rather
// than on the response that came back — the filter string is the part of an audit
// query that is easy to get wrong and impossible to see from the outside.
type entriesAPI interface {
	List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error)
}

// loggingEntries adapts the generated Logging service to entriesAPI. The generated
// call is a builder chain (Entries.List(req).Context(ctx).Do()), which cannot be
// satisfied by a fake; this collapses it to one ordinary method that can.
type loggingEntries struct{ svc *logging.Service }

func (l loggingEntries) List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error) {
	return l.svc.Entries.List(req).Context(ctx).Do()
}

// Client is the GCP cloud provider.
type Client struct {
	entries   entriesAPI
	container *container.Service
	compute   *compute.Service

	project     string
	location    string
	clusterName string
	projectNum  string
	identitySrc string

	maxEvents int64
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
		entries:     loggingEntries{svc: lsvc},
		container:   csvc,
		compute:     msvc,
		project:     id.Project,
		location:    id.Location,
		clusterName: id.ClusterName,
		projectNum:  id.ProjectNumber,
		identitySrc: id.Source,
		maxEvents:   defaultMaxEvents,
	}, nil
}

var (
	_ providers.CloudProvider  = (*Client)(nil)
	_ providers.CloudDescriber = (*Client)(nil)
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
		ResourceNames: []string{"projects/" + c.project},
		Filter:        fmt.Sprintf(`logName="projects/%s/logs/%s"`, c.project, activityLog),
		PageSize:      1,
	})
	if err == nil {
		return nil
	}
	if !deniedStatus(err) {
		return fmt.Errorf("cloud logging read failed on project %s: %w", c.project, err)
	}
	// Lowercase and no trailing period: staticcheck ST1005 and revive's error-strings
	// are both in this repo's gate, and neither exempts "Cloud".
	return fmt.Errorf("cloud logging read denied on project %s: "+
		"RunLore authenticated as ServiceAccount %s/%s, which no GCP role is bound to\n\n"+
		"  gcloud projects add-iam-policy-binding %s \\\n"+
		"    --role=roles/logging.viewer \\\n"+
		"    --member=%q\n\n"+
		"repeat for roles/container.clusterViewer and roles/compute.viewer; "+
		"note the project NUMBER in the member string — the project ID does not work there",
		c.project, podNamespace(), podServiceAccount(),
		c.project, c.principal())
}

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
	num := c.projectNum
	if num == "" {
		num = "<project-number>"
	}
	return fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
		num, c.project, podNamespace(), podServiceAccount())
}

// podNamespace and podServiceAccount read the downward-API values the Helm chart
// injects. They fall back to a placeholder rather than an empty string so a command
// printed outside a pod is visibly a template, instead of a subtly wrong binding that
// looks complete.
func podNamespace() string {
	if v := os.Getenv("POD_NAMESPACE"); v != "" {
		return v
	}
	return "<namespace>"
}

func podServiceAccount() string {
	if v := os.Getenv("POD_SERVICE_ACCOUNT"); v != "" {
		return v
	}
	return "<serviceaccount>"
}
