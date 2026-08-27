// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"

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
