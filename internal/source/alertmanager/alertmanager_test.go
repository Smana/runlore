package alertmanager

import (
	"os"
	"testing"
)

func TestDecodeFiringProducesRequest(t *testing.T) {
	body, _ := os.ReadFile("testdata/firing.json")
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
	if len(res.Resolved) != 0 {
		t.Fatalf("firing must not resolve")
	}
}

func TestDecodeResolvedProducesResolution(t *testing.T) {
	body, _ := os.ReadFile("testdata/resolved.json")
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
}
