// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/gcplog"
	"github.com/Smana/runlore/internal/providers"
)

// preflightClient wires a Client to a fake Cloud Logging that answers every request
// with status and body.
func preflightClient(t *testing.T, id Identity, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), id,
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestPreflightPrintsAPastableBindingOnDenial is the whole reason preflight exists. A
// missing Workload Identity binding otherwise surfaces as a bare 403 partway through an
// investigation, naming an API rather than the binding nobody created. The message has
// to carry the three parts operators most often get wrong: the project NUMBER, the
// namespace and the KSA.
func TestPreflightPrintsAPastableBindingOnDenial(t *testing.T) {
	// Injected rather than set as process environment: pod identity is resolved by
	// internal/app and handed over on Identity, so a test states it the same way
	// production does instead of reaching for t.Setenv.
	c := preflightClient(t, Identity{
		Project:           "my-proj",
		ProjectNumber:     "123456789012",
		PodNamespace:      "runlore",
		PodServiceAccount: "runlore",
	}, http.StatusForbidden, `{"error":{"code":403,"message":"Permission 'logging.logEntries.list' denied"}}`)

	err := c.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight must fail when Cloud Logging denies the read")
	}
	msg := err.Error()
	if !providers.CloudPreflightDenied(err) {
		t.Errorf("a 403 must report as a DENIAL, or the caller cannot tell it from a startup blip "+
			"and will disable the lens for both: %v", err)
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("a denial must carry the summary and the command as separate fields, so the "+
			"command can be written somewhere it stays pastable: %v", err)
	}
	if strings.Contains(denied.Summary, "\n") {
		t.Errorf("the summary is logged as a structured field and must stay single-line, got:\n%s",
			denied.Summary)
	}
	if !strings.Contains(denied.Command, "add-iam-policy-binding") {
		t.Errorf("the command field must carry the pastable binding, got:\n%s", denied.Command)
	}
	for _, want := range []struct{ name, sub string }{
		{"the role to grant is named, so the fix does not need looking up", "roles/logging.viewer"},
		{"the command is pastable rather than described", "add-iam-policy-binding"},
		{"the member string carries the project NUMBER, which is the part that silently never matches when wrong",
			"principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-proj.svc.id.goog/subject/ns/runlore/sa/runlore"},
		{"the other two roles are named, so the operator does not fix one and hit the next", "roles/compute.viewer"},
		{"the identity RunLore authenticated as is stated, since that is what the binding must match", "runlore/runlore"},
	} {
		t.Run(want.name, func(t *testing.T) {
			if !strings.Contains(msg, want.sub) {
				t.Errorf("preflight error does not contain %q:\n%s", want.sub, msg)
			}
		})
	}
}

// TestPreflightRendersAnObviousTemplateOffCluster: run outside a pod the downward-API
// values are absent and projectNum is unresolvable, so the command cannot be correct.
// It must then be visibly a template. A binding string with empty segments is the worse
// failure — gcloud accepts it, and it silently never matches anything.
func TestPreflightRendersAnObviousTemplateOffCluster(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("POD_SERVICE_ACCOUNT", "")

	c := preflightClient(t, Identity{Project: "my-proj"},
		http.StatusForbidden, `{"error":{"code":403,"message":"denied"}}`)

	msg := c.Preflight(context.Background()).Error()
	for _, want := range []string{"<project-number>", "<namespace>", "<serviceaccount>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("off-cluster command is not visibly a template, missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "projects//locations") || strings.Contains(msg, "ns//sa") {
		t.Errorf("an empty segment rendered into the member string, which gcloud accepts and never matches:\n%s", msg)
	}
}

// TestPreflightPassesWhenTheReadSucceeds: the happy path must be silent, or every
// correctly-configured deployment starts with a warning it cannot act on.
func TestPreflightPassesWhenTheReadSucceeds(t *testing.T) {
	c := preflightClient(t, Identity{Project: "my-proj"}, 0, `{"entries":[]}`)
	if err := c.Preflight(context.Background()); err != nil {
		t.Errorf("Preflight failed on a successful read: %v", err)
	}
}

// TestPreflightDoesNotBlameTheBindingForAnOutage separates the two ways the probe can
// fail. A 503 is Google having a bad day; printing an IAM command for it sends the
// operator to add a binding that already exists, and the real fault goes unlooked-at.
func TestPreflightDoesNotBlameTheBindingForAnOutage(t *testing.T) {
	c := preflightClient(t, Identity{Project: "my-proj"},
		http.StatusServiceUnavailable, `{"error":{"code":503,"message":"backend unavailable"}}`)

	err := c.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight must report a failed read")
	}
	if strings.Contains(err.Error(), "add-iam-policy-binding") {
		t.Errorf("a 503 was diagnosed as a missing binding:\n%s", err)
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("the underlying error is not reported, leaving nothing to diagnose:\n%s", err)
	}
}

// fakeEntries is a gcplog.EntriesAPI standing in for Cloud Logging, recording the
// request it was handed and answering with a canned verdict.
//
// It exists alongside the httptest seam above rather than replacing it, because the two
// stage different halves of the contract. httptest answers with an HTTP status, which is
// what a 403 and a 503 are. A fake is the only way to stage the case where there is no
// HTTP response AT ALL — a refused dial, a dropped packet, a deadline — and that case is
// precisely the one the denial/blip split was written for: on Cilium the metadata fetch
// is dropped rather than refused, which is a transport failure and never an HTTP status.
// A test that reached the network to produce it would answer differently depending on
// what credentials the machine running it happened to have.
type fakeEntries struct {
	got  *logging.ListLogEntriesRequest
	resp *logging.ListLogEntriesResponse
	err  error
}

func (f *fakeEntries) List(_ context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &logging.ListLogEntriesResponse{}, nil
}

// fakeEntriesClient wires a Client to e and nothing else. Preflight reads only the
// logging seam, so the container/compute services stay nil here deliberately: a health
// call on this Client must panic rather than quietly reach googleapis.com.
func fakeEntriesClient(id Identity, e gcplog.EntriesAPI) *Client {
	return &Client{entries: e, id: id, maxEvents: defaultMaxEvents}
}

// TestPreflightReportsATransportFailureAsInconclusive pins the half of the denial/blip
// split that has no HTTP status behind it.
//
// deniedStatus answers from a *googleapi.Error's code, so anything that never produced a
// response — a refused dial, a dropped packet, a deadline — must fall through to the
// inconclusive branch. Getting this wrong is expensive and silent: wiring runs once per
// process, so a dial failure classified as a denial disables the cloud lens for the whole
// life of the pod and prints an IAM command for a binding that was already correct.
func TestPreflightReportsATransportFailureAsInconclusive(t *testing.T) {
	// The shape a dropped or refused connection actually arrives in — no googleapi.Error
	// anywhere in the chain, because no HTTP exchange ever completed.
	fake := &fakeEntries{err: &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connect: connection refused"),
	}}
	c := fakeEntriesClient(Identity{Project: "my-proj"}, fake)

	err := c.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight must report a failed read")
	}
	if providers.CloudPreflightDenied(err) {
		t.Errorf("a dial failure was classified as an authorization denial; that disables the "+
			"cloud lens until the pod is restarted, over a blip the provider would have "+
			"survived with no probe at all:\n%s", err)
	}
	if strings.Contains(err.Error(), "add-iam-policy-binding") {
		t.Errorf("a transport failure was diagnosed as a missing binding, sending the operator "+
			"to fix IAM while the real fault goes unlooked-at:\n%s", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the underlying error is not reported, leaving nothing to diagnose:\n%s", err)
	}
}

// TestPreflightDeadlineIsInconclusiveNotDenied covers the failure a dropped packet
// produces once the caller's timeout fires, which is what internal/app bounds the probe
// with. Same verdict as a refused dial, reached by a different path.
func TestPreflightDeadlineIsInconclusiveNotDenied(t *testing.T) {
	c := fakeEntriesClient(Identity{Project: "my-proj"}, &fakeEntries{err: context.DeadlineExceeded})

	err := c.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight must report a failed read")
	}
	if providers.CloudPreflightDenied(err) {
		t.Errorf("a deadline was classified as an authorization denial:\n%s", err)
	}
}

// TestPreflightProbesOneEntryOfTheActivityStream pins the request the probe actually
// builds, which is the thing that decides whether it is cheap and whether it proves
// anything.
//
// PageSize 1 because the probe is a permission check, not a read — a larger page bills
// and delays startup for entries nobody looks at. The activity stream because that is
// the one roles/logging.viewer has to cover for the changes lens to work; probing a
// stream the lens does not read would pass while the lens still 403s. Neither is visible
// from the outside, and the httptest tests above pass regardless of both.
func TestPreflightProbesOneEntryOfTheActivityStream(t *testing.T) {
	fake := &fakeEntries{}
	c := fakeEntriesClient(Identity{Project: "my-proj"}, fake)

	if err := c.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if fake.got == nil {
		t.Fatal("Preflight made no Cloud Logging call, so it proves nothing about the binding")
	}
	if got := fake.got.PageSize; got != 1 {
		t.Errorf("the startup probe asks for %d entries; a permission check needs 1", got)
	}
	if want := []string{"projects/my-proj"}; !slices.Equal(fake.got.ResourceNames, want) {
		t.Errorf("probe scoped to %v, want %v", fake.got.ResourceNames, want)
	}
	if !strings.Contains(fake.got.Filter, activityLog) {
		t.Errorf("the probe does not read the activity stream, so it can pass while the "+
			"changes lens still 403s: %q", fake.got.Filter)
	}
}
