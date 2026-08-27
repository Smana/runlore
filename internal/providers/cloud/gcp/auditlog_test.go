// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// auditFixture builds one audit LogEntry. It is a struct rather than the positional
// argument list the shape invites because the two fields that carry the interesting
// cases — principal and code — are both optional, and a positional helper renders
// their absence as a bare "" and 0 that no reader can tell apart from a value.
//
// Marshalled from typed structs, never a map[string]any: encoding/json sorts map keys,
// so a map fixture emits a field order no GCP API ever produces, and a decoder bug
// that depended on order would be invisible here and live in production.
type auditFixture struct {
	ts        string
	service   string
	method    string
	principal string // empty on a system_event: Google-initiated actions have no caller
	resource  string
	resType   string
	labels    map[string]string
	code      int64 // 0 = the call succeeded and the proto omits status entirely
	message   string
}

// auditLogProto is the wire shape of protoPayload for both audit streams. The pointer
// fields must be pointers: authenticationInfo is absent from every system_event entry
// and status is absent from every successful call, and a fixture that always emits
// them cannot reproduce either case.
type auditLogProto struct {
	Type               string          `json:"@type"`
	ServiceName        string          `json:"serviceName"`
	MethodName         string          `json:"methodName"`
	ResourceName       string          `json:"resourceName"`
	AuthenticationInfo *authInfoProto  `json:"authenticationInfo,omitempty"`
	Status             *rpcStatusProto `json:"status,omitempty"`
}

type authInfoProto struct {
	PrincipalEmail string `json:"principalEmail"`
}

type rpcStatusProto struct {
	Code    int64  `json:"code"`
	Message string `json:"message,omitempty"`
}

func (f auditFixture) entry(t *testing.T) *logging.LogEntry {
	t.Helper()
	p := auditLogProto{
		Type:         "type.googleapis.com/google.cloud.audit.AuditLog",
		ServiceName:  f.service,
		MethodName:   f.method,
		ResourceName: f.resource,
	}
	if f.principal != "" {
		p.AuthenticationInfo = &authInfoProto{PrincipalEmail: f.principal}
	}
	if f.code != 0 {
		p.Status = &rpcStatusProto{Code: f.code, Message: f.message}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal protoPayload: %v", err)
	}
	labels := f.labels
	if labels == nil {
		labels = map[string]string{"location": "europe-west1"}
	}
	return &logging.LogEntry{
		Timestamp:    f.ts,
		InsertId:     "ins-" + f.method + "-" + f.ts,
		ProtoPayload: b,
		Resource:     &logging.MonitoredResource{Type: f.resType, Labels: labels},
	}
}

// entriesServer serves one canned entries.list response to every request and records
// the filter of the last request it saw. The filter is the half of an audit query that
// never appears in the response, so a lens that built the wrong one would satisfy every
// assertion made about what came back.
type entriesServer struct {
	*httptest.Server
	filters []string
}

func serveEntries(t *testing.T, entries ...*logging.LogEntry) (*Client, *entriesServer) {
	t.Helper()
	return serveEntriesFunc(t, func(string) []*logging.LogEntry { return entries })
}

// serveEntriesFunc serves entries chosen from the request's own filter, which is what
// lets one server answer the scoped and unscoped legs of a widen differently.
func serveEntriesFunc(t *testing.T, pick func(filter string) []*logging.LogEntry) (*Client, *entriesServer) {
	t.Helper()
	es := &entriesServer{}
	es.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req logging.ListLogEntriesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode entries.list request: %v", err)
		}
		es.filters = append(es.filters, req.Filter)
		body, err := json.Marshal(logging.ListLogEntriesResponse{Entries: pick(req.Filter)})
		if err != nil {
			t.Errorf("marshal entries.list response: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(es.Close)

	c, err := New(context.Background(),
		Identity{Project: "my-proj", Location: "europe-west1", ClusterName: "prod", ProjectNumber: "123456789012"},
		option.WithHTTPClient(es.Client()),
		option.WithEndpoint(es.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, es
}

// lastFilter returns the filter of the most recent entries.list request.
func (es *entriesServer) lastFilter(t *testing.T) string {
	t.Helper()
	if len(es.filters) == 0 {
		t.Fatal("no entries.list request reached the server")
	}
	return es.filters[len(es.filters)-1]
}

// TestCloudChangesMapsAnAuditEntryOntoTheChangeModel pins every field the
// engine-agnostic timeline reads, so a GCP change joins the same view as a Flux diff
// instead of rendering as a row of blanks. Each field is asserted individually because
// they come from three different parts of the entry — the protoPayload, the
// MonitoredResource and the Client's own scope — and a single struct comparison would
// not say which of the three was wired wrong.
func TestCloudChangesMapsAnAuditEntryOntoTheChangeModel(t *testing.T) {
	const ts = "2026-08-24T10:00:00Z"
	c, _ := serveEntries(t, auditFixture{
		ts:        ts,
		service:   "container.googleapis.com",
		method:    "google.container.v1.ClusterManager.SetNodePoolSize",
		principal: "alice@example.com",
		resource:  "projects/my-proj/locations/europe-west1/clusters/prod/nodePools/default",
		resType:   "gke_nodepool",
	}.entry(t))

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	ch := got[0]
	when, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse want timestamp: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"the engine is GCP, so the timeline can attribute the row", ch.Engine, providers.EngineGCP},
		{"the type is a cloud API call, not a GitOps sync", ch.Type, providers.ChangeCloudAPI},
		{"ManagedBy carries the calling service", ch.ManagedBy, "container.googleapis.com"},
		{"ToRev carries insertId as the model's change_ref handle", ch.ToRev, "ins-google.container.v1.ClusterManager.SetNodePoolSize-" + ts},
		// Engine-qualified, and that prefix is load-bearing rather than cosmetic. A bare
		// "gke_nodepool" carries no character that distinguishes it from a Kubernetes
		// kind, so notify could not tell whether a namespace was a fact about the object
		// and dropped the resource identity from the card entirely. The ':' in the prefix
		// is the one character no Kubernetes kind can contain.
		{"Workload.Kind carries the monitored resource type, engine-qualified",
			ch.Workload.Kind, "gcp::gke_nodepool"},
		{"Workload.Name carries the full resource path", ch.Workload.Name,
			"projects/my-proj/locations/europe-west1/clusters/prod/nodePools/default"},
		{"Workload.Account carries the project the client is scoped to", ch.Workload.Account, "my-proj"},
		{"Workload.Region carries the resource's location label", ch.Workload.Region, "europe-west1"},
		{"Source.Path names the method and the principal that called it", ch.Source.Path,
			"google.container.v1.ClusterManager.SetNodePoolSize by alice@example.com"},
		{"When carries the entry timestamp", ch.When, when},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// TestCloudChangesFallsBackToTheZoneLabelForZonalResources covers the other half of the
// location mapping. A zonal resource (every gce_instance) carries no "location" label
// at all, so a lens that read only that key would render Compute Engine changes with an
// empty region — the field an investigation uses to tell a stockout in one zone from a
// project-wide quota wall.
func TestCloudChangesFallsBackToTheZoneLabelForZonalResources(t *testing.T) {
	c, _ := serveEntries(t, auditFixture{
		ts:        "2026-08-24T10:00:00Z",
		service:   "compute.googleapis.com",
		method:    "v1.compute.instances.insert",
		principal: "alice@example.com",
		resource:  "projects/my-proj/zones/europe-west1-b/instances/node-1",
		resType:   "gce_instance",
		labels:    map[string]string{"zone": "europe-west1-b", "instance_id": "8371"},
	}.entry(t))

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	if got[0].Workload.Region != "europe-west1-b" {
		t.Errorf("Workload.Region = %q, want the zone label europe-west1-b", got[0].Workload.Region)
	}
}

// TestCloudChangesNamesTheRPCCodeOfAFailedCall is the highest-value mapping in this
// lens: a denied or quota-exhausted call rendered as a success has the model conclude
// the change took effect, and then reason forward from a state the cloud never reached.
//
// The name matters as much as the failure. "status code 7" is a number a model has to
// guess at; PERMISSION_DENIED is a diagnosis. An unmapped number still has to render as
// something a reader can act on rather than vanishing.
func TestCloudChangesNamesTheRPCCodeOfAFailedCall(t *testing.T) {
	tests := []struct {
		name    string
		code    int64
		message string
		want    string
	}{
		{
			name:    "a quota wall renders as RESOURCE_EXHAUSTED with the quota that was hit",
			code:    8,
			message: "Quota 'CPUS' exceeded. Limit: 24.0",
			want:    " — FAILED: RESOURCE_EXHAUSTED (Quota 'CPUS' exceeded. Limit: 24.0)",
		},
		{
			name:    "a missing IAM binding renders as PERMISSION_DENIED",
			code:    7,
			message: "Required 'compute.instances.create' permission",
			want:    " — FAILED: PERMISSION_DENIED (Required 'compute.instances.create' permission)",
		},
		{
			name: "a failure with no message still says it failed",
			code: 5,
			want: " — FAILED: NOT_FOUND",
		},
		{
			name:    "a code outside google.rpc.Code renders the number rather than disappearing",
			code:    99,
			message: "something new",
			want:    " — FAILED: code 99 (something new)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := serveEntries(t, auditFixture{
				ts:        "2026-08-24T10:00:00Z",
				service:   "compute.googleapis.com",
				method:    "v1.compute.instances.insert",
				principal: "svc@my-proj.iam.gserviceaccount.com",
				resource:  "projects/my-proj/zones/europe-west1-b/instances/node-1",
				resType:   "gce_instance",
				code:      tt.code,
				message:   tt.message,
			}.entry(t))

			got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
			if err != nil {
				t.Fatalf("CloudChanges: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d changes, want 1", len(got))
			}
			want := "v1.compute.instances.insert by svc@my-proj.iam.gserviceaccount.com" + tt.want
			if got[0].Source.Path != want {
				t.Errorf("Source.Path =\n  %q\nwant\n  %q", got[0].Source.Path, want)
			}
		})
	}
}

// TestCloudChangesReadsBothAuditStreams pins the filter, which is the half of the query
// no response can reveal.
//
// system_event is the stream with no AWS equivalent: it carries host error, live
// migration and preemption — Google-initiated actions on a node, and often the whole
// answer to "why did this pod restart". A filter naming only the activity log returns a
// perfectly plausible list of changes with those entries silently missing.
func TestCloudChangesReadsBothAuditStreams(t *testing.T) {
	c, es := serveEntries(t)
	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	if _, err := c.CloudChanges(context.Background(),
		providers.Selector{Name: "my-nodepool"},
		providers.TimeWindow{Start: start, End: start.Add(time.Hour)},
		providers.CloudChangeFilter{},
	); err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}

	filter := es.lastFilter(t)
	for _, want := range []string{
		`"projects/my-proj/logs/cloudaudit.googleapis.com%2Factivity"`,
		`"projects/my-proj/logs/cloudaudit.googleapis.com%2Fsystem_event"`,
		`protoPayload.resourceName:"my-nodepool"`,
		`timestamp>="2026-08-24T09:00:00Z"`,
		`timestamp<="2026-08-24T10:00:00Z"`,
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter does not contain %s:\n%s", want, filter)
		}
	}
	// data_access is off by default outside BigQuery, dominated by reads, and needs a
	// materially wider IAM grant (roles/logging.privateLogViewer) to read at all.
	if strings.Contains(filter, "data_access") {
		t.Errorf("filter reads the data_access log, which this lens deliberately does not:\n%s", filter)
	}
}

// TestCloudChangesLeavesTheFilterUnscopedWhenTheSelectorIsEmpty is the other half of
// the scoping contract. A lens that spliced an empty resource name in would emit
// protoPayload.resourceName:"" — a clause the model never asked for, whose match
// semantics against every entry in the project are nobody's intent.
func TestCloudChangesLeavesTheFilterUnscopedWhenTheSelectorIsEmpty(t *testing.T) {
	c, es := serveEntries(t)
	if _, err := c.CloudChanges(context.Background(), providers.Selector{},
		providers.TimeWindow{}, providers.CloudChangeFilter{}); err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if filter := es.lastFilter(t); strings.Contains(filter, "resourceName") {
		t.Errorf("an unscoped lookup still filtered on a resource name:\n%s", filter)
	}
}

// TestCloudChangesRendersASystemEventWithoutInventingAPrincipal covers the entry shape
// unique to GCP. A host error is Google-initiated, so the AuditLog proto carries no
// authenticationInfo at all — and "compute.instances.hostError by " reads to a model as
// a caller whose identity was lost, which is a different and much more alarming claim
// than "nobody called this".
func TestCloudChangesRendersASystemEventWithoutInventingAPrincipal(t *testing.T) {
	c, _ := serveEntries(t, auditFixture{
		ts:       "2026-08-24T10:00:00Z",
		service:  "compute.googleapis.com",
		method:   "compute.instances.hostError",
		resource: "projects/my-proj/zones/europe-west1-b/instances/gke-prod-pool-a1b2c3",
		resType:  "gce_instance",
		labels:   map[string]string{"zone": "europe-west1-b"},
	}.entry(t))

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	if got[0].Source.Path != "compute.instances.hostError" {
		t.Errorf("Source.Path = %q, want the bare method with no dangling principal", got[0].Source.Path)
	}
}

// TestCloudChangesAppendsTheTruncationNoteLast pins ordering, not just presence. The
// note carries no When, so appending it before the sort would place it among events
// from 1970 — at the very end of a newest-first list on a good day, and in the middle
// of one as soon as anything else lacks a timestamp.
func TestCloudChangesAppendsTheTruncationNoteLast(t *testing.T) {
	var entries []*logging.LogEntry
	for i := 0; i < defaultMaxEvents+5; i++ {
		entries = append(entries, auditFixture{
			ts:        time.Date(2026, 8, 24, 10, 0, i, 0, time.UTC).Format(time.RFC3339),
			service:   "compute.googleapis.com",
			method:    fmt.Sprintf("v1.compute.instances.insert.%d", i),
			principal: "alice@example.com",
			resource:  "projects/my-proj/zones/europe-west1-b/instances/node-1",
			resType:   "gce_instance",
		}.entry(t))
	}
	c, _ := serveEntries(t, entries...)

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != defaultMaxEvents+1 {
		t.Fatalf("got %d changes, want %d (the cap plus one note)", len(got), defaultMaxEvents+1)
	}
	last := got[len(got)-1]
	if !providers.IsChangeNote(last) {
		t.Fatalf("the last change is not the truncation note: %+v", last)
	}
	if last.Engine != providers.EngineGCP {
		t.Errorf("the note is tagged %q, want %q", last.Engine, providers.EngineGCP)
	}
	if !strings.Contains(last.Workload.Name, "truncated") {
		t.Errorf("the note does not say the view is partial: %q", last.Workload.Name)
	}
	for i, ch := range got[:len(got)-1] {
		if providers.IsChangeNote(ch) {
			t.Fatalf("a note sorted into position %d, among the real events", i)
		}
		if i > 0 && !got[i-1].When.After(ch.When) {
			t.Errorf("changes are not newest-first at %d: %v then %v", i, got[i-1].When, ch.When)
		}
	}
	// The cap keeps the NEWEST events. Keeping the oldest would answer "what changed"
	// with the events furthest from the incident.
	newest := time.Date(2026, 8, 24, 10, 0, defaultMaxEvents+4, 0, time.UTC)
	if !got[0].When.Equal(newest) {
		t.Errorf("the newest kept event is %v, want %v", got[0].When, newest)
	}
}

// TestCloudChangesPushesFailedOnlyIntoTheServerSideFilter pins where the failure filter
// runs, which on GCP is the whole difference from the AWS lens.
//
// CloudTrail accepts exactly one LookupAttribute and it is already spent, so AWS filters
// rejected calls client-side and has to bound the scan (maxFailureScanPages) — a sparse
// failure can sit behind pages of successful churn. Cloud Logging has no such limit, so
// the clause goes in the filter: the cap is then spent entirely on rejected calls, and
// there is no scan budget to bound and no budget note to explain.
func TestCloudChangesPushesFailedOnlyIntoTheServerSideFilter(t *testing.T) {
	c, es := serveEntries(t)
	if _, err := c.CloudChanges(context.Background(), providers.Selector{},
		providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true}); err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if filter := es.lastFilter(t); !strings.Contains(filter, "protoPayload.status.code!=0") {
		t.Errorf("failed_only did not reach the filter, so it would be silently ignored:\n%s", filter)
	}

	c2, es2 := serveEntries(t)
	if _, err := c2.CloudChanges(context.Background(), providers.Selector{},
		providers.TimeWindow{}, providers.CloudChangeFilter{}); err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if filter := es2.lastFilter(t); strings.Contains(filter, "status.code") {
		t.Errorf("an unfiltered lookup narrowed itself to rejected calls:\n%s", filter)
	}
}

// TestCloudChangesNeverReportsASuccessfulCallUnderFailedOnly guards the answer rather
// than the query. The filter clause is what makes failed_only cheap, but it is a
// promise made by the service about how != treats an absent field, and a successful
// AuditLog omits status entirely. If that promise ever bends, every routine call in the
// window arrives labelled as a rejected one — the exact inversion the flag exists to
// prevent — so the status the rendering keys on is also the status the filter keys on.
func TestCloudChangesNeverReportsASuccessfulCallUnderFailedOnly(t *testing.T) {
	c, _ := serveEntries(t,
		auditFixture{
			ts: "2026-08-24T10:00:01Z", service: "compute.googleapis.com",
			method: "v1.compute.instances.insert", principal: "alice@example.com",
			resource: "projects/my-proj/zones/europe-west1-b/instances/ok", resType: "gce_instance",
		}.entry(t),
		auditFixture{
			ts: "2026-08-24T10:00:00Z", service: "compute.googleapis.com",
			method: "v1.compute.instances.insert", principal: "alice@example.com",
			resource: "projects/my-proj/zones/europe-west1-b/instances/denied", resType: "gce_instance",
			code: 7, message: "denied",
		}.entry(t),
	)

	got, err := c.CloudChanges(context.Background(), providers.Selector{},
		providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want only the rejected one: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Source.Path, "PERMISSION_DENIED") {
		t.Errorf("the kept change is not the rejected call: %q", got[0].Source.Path)
	}
}

// TestCloudChangesReportsAQuietWindowAsQuietRatherThanPartial pins that an empty result
// carries no note. The tools read a note-only slice as "the lookup stopped reading" and
// suppress the "no mutating GCP events" message; a note emitted on an empty window would
// turn every quiet window into an inconclusive one.
func TestCloudChangesReportsAQuietWindowAsQuietRatherThanPartial(t *testing.T) {
	c, _ := serveEntries(t)
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty window produced %d changes: %+v", len(got), got)
	}
}

// TestAScopedMissWidensAndExplainsItWithGCPsOwnRule is the one assertion neither layer
// can make alone.
//
// internal/investigate owns the widen, and its own tests drive it through a hand-written
// vocabulary fixture because that package must not import a concrete cloud provider —
// so they prove the tool consults A vocabulary, never that THIS provider's scoped miss
// reaches the widen at all. This package owns the vocabulary and the empty result, but
// neither of them renders a banner. Only the composition shows a GCP operator what a
// missed scope actually prints.
//
// What it must not print is AWS's lecture. On CloudTrail a scoped miss usually means the
// name was written in the wrong form, and the banner says so; Cloud Logging's ':' is a
// substring match, so a miss here means the resource genuinely did not appear, and
// telling the model to try another spelling sends it renaming a resource it had already
// identified correctly.
func TestAScopedMissWidensAndExplainsItWithGCPsOwnRule(t *testing.T) {
	c, es := serveEntriesFunc(t, func(filter string) []*logging.LogEntry {
		if strings.Contains(filter, "resourceName") {
			return nil // the scoped leg misses
		}
		return []*logging.LogEntry{auditFixture{
			ts:        time.Now().UTC().Format(time.RFC3339),
			service:   "container.googleapis.com",
			method:    "google.container.v1.ClusterManager.UpdateCluster",
			principal: "alice@example.com",
			resource:  "projects/my-proj/locations/europe-west1/clusters/prod",
			resType:   "gke_cluster",
		}.entry(t)}
	})

	out, err := investigate.CloudWhatChangedTool{Cloud: c}.Call(context.Background(), `{"resource":"guessed"}`)
	if err != nil {
		t.Fatalf("cloud_what_changed: %v", err)
	}
	if len(es.filters) != 2 {
		t.Fatalf("saw %d lookups, want a scoped miss followed by an unscoped retry", len(es.filters))
	}
	if !strings.Contains(out, "SUBSTRING match on protoPayload.resourceName") {
		t.Errorf("the widen banner does not explain GCP's own match rule:\n%s", out)
	}
	if strings.Contains(out, "CloudTrail") || strings.Contains(out, "exact match") {
		t.Errorf("the widen banner gives a GCP operator AWS's advice:\n%s", out)
	}
	if !strings.Contains(out, "google.container.v1.ClusterManager.UpdateCluster") {
		t.Errorf("the widened result dropped the events it widened to find:\n%s", out)
	}
}

// TestAScopedHitIsNotWidened is the companion. The widen is a fallback, and a fallback
// that fires on a lookup that already matched would tell the model its filter had been
// dropped when it had not — and hand it other resources' changes as that resource's.
func TestAScopedHitIsNotWidened(t *testing.T) {
	c, es := serveEntriesFunc(t, func(filter string) []*logging.LogEntry {
		if !strings.Contains(filter, `protoPayload.resourceName:"gke-prod-pool-a1b2c3"`) {
			t.Errorf("the scoped lookup did not carry the resource:\n%s", filter)
		}
		return []*logging.LogEntry{auditFixture{
			ts:        time.Now().UTC().Format(time.RFC3339),
			service:   "compute.googleapis.com",
			method:    "v1.compute.instances.delete",
			principal: "alice@example.com",
			resource:  "projects/my-proj/zones/europe-west1-b/instances/gke-prod-pool-a1b2c3",
			resType:   "gce_instance",
			labels:    map[string]string{"zone": "europe-west1-b"},
		}.entry(t)}
	})

	out, err := investigate.CloudWhatChangedTool{Cloud: c}.Call(context.Background(),
		`{"resource":"gke-prod-pool-a1b2c3"}`)
	if err != nil {
		t.Fatalf("cloud_what_changed: %v", err)
	}
	if len(es.filters) != 1 {
		t.Errorf("a scoped lookup that matched was retried unscoped: %d lookups", len(es.filters))
	}
	if strings.Contains(out, "matched no Cloud Audit Log entries") {
		t.Errorf("a scope that matched was reported as dropped:\n%s", out)
	}
	if !strings.Contains(out, "v1.compute.instances.delete") {
		t.Errorf("the scoped result dropped its own event:\n%s", out)
	}
}
