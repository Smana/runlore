package alertmanager

import (
	"os"
	"testing"
)

func TestDecodeFiringProducesRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/firing.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Title != "HighMem" || r.Severity != "critical" || r.Workload.Namespace != "prod-web" || r.Workload.Name != "web" {
		t.Fatalf("bad request: %+v", r)
	}
	if r.Workload.Kind != "Deployment" {
		t.Fatalf("want Workload.Kind=Deployment, got %q", r.Workload.Kind)
	}
	if len(res.Resolved) != 0 {
		t.Fatalf("firing must not resolve")
	}
}

func TestDecodeResolvedProducesResolution(t *testing.T) {
	body, err := os.ReadFile("testdata/resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 0 {
		t.Fatalf("resolved must not enqueue")
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Fingerprint != "abc" {
		t.Fatalf("want resolved abc, got %+v", res.Resolved)
	}
	if res.Resolved[0].At.IsZero() {
		t.Fatal("Resolution.At must not be zero (should be receipt time)")
	}
}
